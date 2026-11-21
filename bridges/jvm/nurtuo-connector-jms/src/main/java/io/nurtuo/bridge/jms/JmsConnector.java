// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.jms;

import io.nurtuo.bridge.sdk.ApplicationException;
import io.nurtuo.bridge.sdk.Connector;
import io.nurtuo.bridge.sdk.ConnectorException;
import io.nurtuo.bridge.sdk.Documents;
import io.nurtuo.bridge.sdk.ExecuteContext;
import io.nurtuo.bridge.sdk.InputDocument;
import io.nurtuo.bridge.sdk.ResultSink;
import io.nurtuo.bridge.sdk.Settings;
import io.nurtuo.bridge.sdk.Structs;
import io.nurtuo.bridge.sdk.TestResult;
import jakarta.jms.BytesMessage;
import jakarta.jms.ConnectionMetaData;
import jakarta.jms.Destination;
import jakarta.jms.InvalidDestinationRuntimeException;
import jakarta.jms.JMSConsumer;
import jakarta.jms.JMSContext;
import jakarta.jms.JMSProducer;
import jakarta.jms.JMSRuntimeException;
import jakarta.jms.MapMessage;
import jakarta.jms.Message;
import jakarta.jms.MessageFormatRuntimeException;
import jakarta.jms.ObjectMessage;
import jakarta.jms.Queue;
import jakarta.jms.StreamMessage;
import jakarta.jms.TextMessage;
import jakarta.jms.Topic;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Enumeration;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.Set;
import java.util.UUID;
import java.util.concurrent.TimeUnit;
import nurtuo.connector.v1.Connector.Capabilities;
import nurtuo.connector.v1.Connector.Descriptor;
import nurtuo.connector.v1.Connector.Host;
import nurtuo.connector.v1.Connector.OperationDescriptor;
import nurtuo.connector.v1.Connector.OperationType;

/**
 * JMS 2.0 (Jakarta Messaging) connector: SEND to a queue/topic, RECEIVE from a queue/topic and
 * REQUEST_REPLY. All three are {@code OPERATION_TYPE_EXECUTE} with the operation in
 * {@code custom_type}. One JMS connection/context is opened per Execute batch and closed at its
 * end; the provider comes from {@link JmsConnectionFactories}.
 *
 * <p>Delivery semantics: SEND uses an auto-acknowledging context (a persistent send returns after
 * the broker accepted it). RECEIVE uses a transacted context that is committed after the output
 * documents of one trigger document were emitted and rolled back if that document fails, so a
 * failing trigger leaves its messages on the broker (at-least-once towards the runtime).
 */
public final class JmsConnector extends Connector {
    public static final String ID = "jms";
    public static final String VERSION = "0.1.0";

    @Override
    public Descriptor describe() {
        return Descriptor.newBuilder()
                .setId(ID)
                .setVersion(VERSION)
                .setHost(Host.HOST_BRIDGE_JVM)
                .setDisplayName("JMS 2.0 (Artemis, IBM MQ, any Jakarta JMS provider)")
                .setConnectionSchema(connectionSchema())
                .addOperations(operation(JmsOptions.SEND, sendSchema()))
                .addOperations(operation(JmsOptions.RECEIVE, receiveSchema()))
                .addOperations(operation(JmsOptions.REQUEST_REPLY, requestReplySchema()))
                .addTrackedProperties("jms.messageId")
                .addTrackedProperties("jms.correlationId")
                .addTrackedProperties("jms.received")
                .setCapabilities(Capabilities.newBuilder()
                        .setStreaming(true)
                        .setMaxBatchSize(1000)
                        .setTransactional(false))
                .build();
    }

    @Override
    public Set<String> secretKeys() {
        return Set.of("password");
    }

    @Override
    public TestResult test(Settings connection) {
        try (JMSContext jms = JmsConnectionFactories.openContext(connection, JMSContext.AUTO_ACKNOWLEDGE)) {
            ConnectionMetaData md = jms.getMetaData();
            return TestResult.ok("connected to " + md.getJMSProviderName() + " " + md.getProviderVersion()
                    + " (JMS " + md.getJMSVersion() + ")");
        } catch (jakarta.jms.JMSException e) {
            return TestResult.failed("JMS provider metadata unavailable: " + e);
        }
    }

    @Override
    public void execute(ExecuteContext ctx, Documents docs, ResultSink sink) throws Exception {
        if (ctx.operationType() != OperationType.OPERATION_TYPE_EXECUTE) {
            throw ConnectorException.invalidArgument("the JMS connector only supports EXECUTE operations");
        }
        JmsOptions opts = JmsOptions.parse(ctx.customType(), ctx.options());
        int mode = opts.operation.equals(JmsOptions.RECEIVE) ? JMSContext.SESSION_TRANSACTED : JMSContext.AUTO_ACKNOWLEDGE;
        JMSContext jms;
        try {
            jms = JmsConnectionFactories.openContext(ctx.connection(), mode);
        } catch (JMSRuntimeException e) {
            // Broker unreachable / bad credentials: technical failure of every document, so the
            // flow's per-document retry/error handling applies.
            String msg = "cannot connect to JMS provider: " + ctx.redactor().redact(e);
            InputDocument d;
            while ((d = docs.next()) != null) {
                sink.failure(d.id(), msg);
            }
            return;
        }
        try (Batch batch = new Batch(jms, opts)) {
            InputDocument doc;
            while ((doc = docs.next()) != null) {
                if (ctx.isCancelled()) {
                    return;
                }
                try {
                    batch.handle(doc, sink);
                } catch (ApplicationException e) {
                    batch.rollbackIfTransacted();
                    sink.applicationError(doc.id(), e.getMessage());
                } catch (ConnectorException | InterruptedException e) {
                    throw e;
                } catch (InvalidDestinationRuntimeException | MessageFormatRuntimeException e) {
                    batch.rollbackIfTransacted();
                    sink.applicationError(doc.id(), ctx.redactor().redact(e));
                } catch (Exception e) {
                    batch.rollbackIfTransacted();
                    sink.failure(doc.id(), ctx.redactor().redact(e));
                }
            }
        }
    }

    /** State of one Execute batch over one JMS context. */
    private static final class Batch implements AutoCloseable {
        private final JMSContext jms;
        private final JmsOptions o;
        private Destination destination;
        private JMSProducer producer;
        private JMSConsumer consumer; // RECEIVE consumer or shared reply consumer
        private Queue tempReplyQueue;

        Batch(JMSContext jms, JmsOptions o) {
            this.jms = jms;
            this.o = o;
            try {
                this.destination = o.destination(jms);
                if (!o.operation.equals(JmsOptions.RECEIVE)) {
                    producer = jms.createProducer()
                            .setDeliveryMode(o.deliveryMode)
                            .setPriority(o.priority)
                            .setTimeToLive(o.timeToLiveMs);
                }
                if (o.operation.equals(JmsOptions.RECEIVE)) {
                    consumer = createReceiveConsumer();
                } else if (o.operation.equals(JmsOptions.REQUEST_REPLY) && o.replyQueue == null) {
                    tempReplyQueue = jms.createTemporaryQueue();
                    consumer = jms.createConsumer(tempReplyQueue);
                }
            } catch (RuntimeException e) {
                jms.close();
                throw e;
            }
        }

        private JMSConsumer createReceiveConsumer() {
            if (o.subscriptionName != null) {
                return jms.createDurableConsumer((Topic) destination, o.subscriptionName, o.selector, false);
            }
            return o.selector == null ? jms.createConsumer(destination) : jms.createConsumer(destination, o.selector);
        }

        void handle(InputDocument doc, ResultSink sink) throws Exception {
            switch (o.operation) {
                case JmsOptions.SEND:
                    send(doc, sink);
                    break;
                case JmsOptions.RECEIVE:
                    receive(doc, sink);
                    break;
                default:
                    requestReply(doc, sink);
            }
        }

        void rollbackIfTransacted() {
            if (o.operation.equals(JmsOptions.RECEIVE)) {
                try {
                    jms.rollback();
                } catch (RuntimeException e) {
                    // connection is broken; the broker redelivers uncommitted messages
                }
            }
        }

        private void send(InputDocument doc, ResultSink sink) throws Exception {
            Message m = buildMessage(doc);
            producer.send(destination, m);
            sink.success(doc.id(), tracked(m), List.of());
        }

        private void receive(InputDocument doc, ResultSink sink) throws Exception {
            List<String> outIds = new ArrayList<>();
            int n = 0;
            while (n < o.maxMessages) {
                Message m;
                if (n == 0) {
                    m = o.timeoutMs > 0 ? consumer.receive(o.timeoutMs) : consumer.receiveNoWait();
                } else {
                    m = consumer.receiveNoWait();
                }
                if (m == null) {
                    break;
                }
                n++;
                String outId = doc.id() + "-" + n;
                emit(sink, outId, m);
                outIds.add(outId);
            }
            jms.commit();
            sink.success(doc.id(), Map.of("jms.received", String.valueOf(n)), outIds);
        }

        private void requestReply(InputDocument doc, ResultSink sink) throws Exception {
            Message req = buildMessage(doc);
            Queue replyTo = tempReplyQueue != null ? tempReplyQueue : jms.createQueue(o.replyQueue);
            String corr = req.getJMSCorrelationID() != null ? req.getJMSCorrelationID() : UUID.randomUUID().toString();
            req.setJMSCorrelationID(corr);
            req.setJMSReplyTo(replyTo);
            producer.send(destination, req);
            String msgId = req.getJMSMessageID();

            boolean perRequestConsumer = tempReplyQueue == null;
            JMSConsumer rc = perRequestConsumer
                    ? jms.createConsumer(replyTo, "JMSCorrelationID='" + esc(corr) + "' OR JMSCorrelationID='" + esc(msgId) + "'")
                    : consumer;
            Message reply = null;
            try {
                long deadline = System.nanoTime() + TimeUnit.MILLISECONDS.toNanos(Math.max(1, o.timeoutMs));
                while (true) {
                    long remainingMs = TimeUnit.NANOSECONDS.toMillis(deadline - System.nanoTime());
                    if (remainingMs <= 0) {
                        break;
                    }
                    Message r = rc.receive(remainingMs);
                    if (r == null) {
                        break;
                    }
                    // A late reply to an earlier, timed-out request may still arrive on the shared
                    // temporary queue; only accept ours (correlation id or request message id).
                    String rc2 = r.getJMSCorrelationID();
                    if (perRequestConsumer || corr.equals(rc2) || (msgId != null && msgId.equals(rc2))) {
                        reply = r;
                        break;
                    }
                }
            } finally {
                if (perRequestConsumer) {
                    rc.close();
                }
            }
            if (reply == null) {
                sink.failure(doc.id(), "no reply within " + o.timeoutMs + " ms");
                return;
            }
            String outId = doc.id() + "-reply";
            emit(sink, outId, reply);
            sink.success(doc.id(), tracked(req), List.of(outId));
        }

        private Message buildMessage(InputDocument doc) throws Exception {
            byte[] payload = doc.readAll();
            Message m;
            if (o.textMessages) {
                m = jms.createTextMessage(new String(payload, StandardCharsets.UTF_8));
            } else {
                BytesMessage b = jms.createBytesMessage();
                b.writeBytes(payload);
                m = b;
            }
            String corr = o.correlationId;
            for (Map.Entry<String, String> p : doc.properties().entrySet()) {
                String k = p.getKey();
                String v = p.getValue();
                if (k.startsWith("nurtuo.")) {
                    continue;
                }
                if (k.equals("jms.correlationId")) {
                    corr = v;
                } else if (k.equals("jms.type")) {
                    m.setJMSType(v);
                } else if (k.startsWith("jms.")) {
                    throw new ApplicationException("unsupported reserved document property '" + k
                            + "' (supported: jms.correlationId, jms.type)");
                } else {
                    String name = propertyName(k);
                    if (name != null) {
                        m.setStringProperty(name, v);
                    }
                }
            }
            if (corr != null) {
                m.setJMSCorrelationID(corr);
            }
            return m;
        }

        private static Map<String, String> tracked(Message m) throws jakarta.jms.JMSException {
            Map<String, String> t = new LinkedHashMap<>();
            if (m.getJMSMessageID() != null) {
                t.put("jms.messageId", m.getJMSMessageID());
            }
            if (m.getJMSCorrelationID() != null) {
                t.put("jms.correlationId", m.getJMSCorrelationID());
            }
            return t;
        }

        private static void emit(ResultSink sink, String outId, Message m) throws Exception {
            Map<String, String> props = new LinkedHashMap<>();
            byte[] body = new byte[0];
            String type;
            if (m instanceof TextMessage) {
                String t = m.getBody(String.class);
                body = t == null ? body : t.getBytes(StandardCharsets.UTF_8);
                type = "text";
            } else if (m instanceof BytesMessage) {
                byte[] b = m.getBody(byte[].class);
                body = b == null ? body : b;
                type = "bytes";
            } else if (m instanceof MapMessage) {
                type = "map"; // body not representable as bytes; properties and headers are still delivered
            } else if (m instanceof ObjectMessage) {
                type = "object";
            } else if (m instanceof StreamMessage) {
                type = "stream";
            } else {
                type = "message";
            }
            Enumeration<?> names = m.getPropertyNames();
            while (names.hasMoreElements()) {
                String name = String.valueOf(names.nextElement());
                props.put(name, String.valueOf(m.getObjectProperty(name)));
            }
            props.put("jms.bodyType", type);
            putIfPresent(props, "jms.messageId", m.getJMSMessageID());
            putIfPresent(props, "jms.correlationId", m.getJMSCorrelationID());
            putIfPresent(props, "jms.type", m.getJMSType());
            props.put("jms.timestamp", String.valueOf(m.getJMSTimestamp()));
            props.put("jms.redelivered", String.valueOf(m.getJMSRedelivered()));
            Destination d = m.getJMSDestination();
            if (d instanceof Queue) {
                props.put("jms.destination", ((Queue) d).getQueueName());
            } else if (d instanceof Topic) {
                props.put("jms.destination", ((Topic) d).getTopicName());
            }
            sink.output(outId, props, body);
        }

        private static void putIfPresent(Map<String, String> m, String k, String v) {
            if (v != null) {
                m.put(k, v);
            }
        }

        @Override
        public void close() {
            jms.close();
        }
    }

    /** JMS property names are Java identifiers and must not start with JMS; sanitize arbitrary keys. */
    static String propertyName(String key) {
        StringBuilder sb = new StringBuilder();
        for (int i = 0; i < key.length(); i++) {
            char c = key.charAt(i);
            sb.append(Character.isJavaIdentifierPart(c) ? c : '_');
        }
        if (sb.length() == 0) {
            return null;
        }
        if (!Character.isJavaIdentifierStart(sb.charAt(0))) {
            sb.insert(0, '_');
        }
        String name = sb.toString();
        if (name.regionMatches(true, 0, "JMS", 0, 3)) {
            return "x_" + name;
        }
        switch (name.toUpperCase(Locale.ROOT)) {
            case "NULL":
            case "TRUE":
            case "FALSE":
            case "NOT":
            case "AND":
            case "OR":
            case "BETWEEN":
            case "LIKE":
            case "IN":
            case "IS":
            case "ESCAPE":
                return "x_" + name;
            default:
                return name;
        }
    }

    private static String esc(String s) {
        return s.replace("'", "''");
    }

    // ---- descriptor ----

    private static OperationDescriptor operation(String customType, com.google.protobuf.Struct schema) {
        return OperationDescriptor.newBuilder()
                .setType(OperationType.OPERATION_TYPE_EXECUTE)
                .setCustomType(customType)
                .setSupportsBrowse(false)
                .setOptionsSchema(schema)
                .build();
    }

    private static Map<String, Object> str(String description) {
        return Map.of("type", "string", "description", description);
    }

    private static com.google.protobuf.Struct connectionSchema() {
        Map<String, Object> props = new LinkedHashMap<>();
        props.put("provider", Map.of("type", "string", "enum", List.of("artemis", "ibmmq", "custom"), "default", "artemis"));
        props.put("brokerUrl", str("Provider URL, e.g. tcp://host:61616 (Artemis, required) or the String constructor argument of a custom factory"));
        props.put("factoryClass", str("Override the ConnectionFactory class; the jar must be on the plugin classpath"));
        props.put("factoryProperties", Map.of(
                "type", "object",
                "description", "Bean properties applied to the factory, e.g. hostName, port, queueManager, channel for IBM MQ",
                "additionalProperties", true));
        props.put("username", str("Optional user name"));
        props.put("password", Map.of("type", "string", "description", "Secret reference, injected in memory only", "format", "secret"));
        props.put("clientId", str("Optional JMS client id (needed for durable topic subscriptions)"));
        return Structs.of("type", "object", "properties", props, "additionalProperties", false);
    }

    private static Map<String, Object> commonProps() {
        Map<String, Object> props = new LinkedHashMap<>();
        props.put("destination", str("Queue or topic name"));
        props.put("destinationType", Map.of("type", "string", "enum", List.of("queue", "topic"), "default", "queue"));
        return props;
    }

    private static com.google.protobuf.Struct sendSchema() {
        Map<String, Object> props = commonProps();
        props.put("messageType", Map.of("type", "string", "enum", List.of("text", "bytes"), "default", "text"));
        props.put("deliveryMode", Map.of("type", "string", "enum", List.of("persistent", "non_persistent"), "default", "persistent"));
        props.put("priority", Map.of("type", "integer", "minimum", 0, "maximum", 9, "default", 4));
        props.put("timeToLiveMs", Map.of("type", "integer", "minimum", 0, "default", 0));
        props.put("correlationId", str("JMSCorrelationID; a document property jms.correlationId overrides it"));
        return Structs.of("type", "object", "required", List.of("destination"), "properties", props);
    }

    private static com.google.protobuf.Struct receiveSchema() {
        Map<String, Object> props = commonProps();
        props.put("maxMessages", Map.of("type", "integer", "minimum", 1, "maximum", JmsOptions.MAX_MESSAGES_CAP, "default", 1));
        props.put("timeoutMs", Map.of("type", "integer", "minimum", 0, "default", 1000));
        props.put("selector", str("JMS message selector"));
        props.put("subscriptionName", str("Durable subscription name (topics; needs clientId in the connection)"));
        return Structs.of("type", "object", "required", List.of("destination"), "properties", props);
    }

    private static com.google.protobuf.Struct requestReplySchema() {
        Map<String, Object> props = commonProps();
        props.put("messageType", Map.of("type", "string", "enum", List.of("text", "bytes"), "default", "text"));
        props.put("timeoutMs", Map.of("type", "integer", "minimum", 1, "default", 30000));
        props.put("replyQueue", str("Named reply queue; default is a temporary queue per batch"));
        props.put("correlationId", str("Correlation id; default a random UUID"));
        return Structs.of("type", "object", "required", List.of("destination"), "properties", props);
    }
}

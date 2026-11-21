// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.jms;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

import io.grpc.Status;
import io.nurtuo.bridge.sdk.Structs;
import io.nurtuo.bridge.sdk.testing.ConnectorTestHarness;
import io.nurtuo.bridge.sdk.testing.ConnectorTestHarness.Doc;
import io.nurtuo.bridge.sdk.testing.ConnectorTestHarness.Outcome;
import jakarta.jms.JMSConsumer;
import jakarta.jms.JMSContext;
import jakarta.jms.Message;
import jakarta.jms.TextMessage;
import java.util.List;
import java.util.Map;
import java.util.concurrent.atomic.AtomicReference;
import nurtuo.connector.v1.Connector.DocumentStatus;
import nurtuo.connector.v1.Connector.ExecuteStart;
import nurtuo.connector.v1.Connector.OperationType;
import nurtuo.connector.v1.Connector.TestRequest;
import org.apache.activemq.artemis.core.config.Configuration;
import org.apache.activemq.artemis.core.config.impl.ConfigurationImpl;
import org.apache.activemq.artemis.core.server.embedded.EmbeddedActiveMQ;
import org.apache.activemq.artemis.jms.client.ActiveMQJMSConnectionFactory;
import org.junit.jupiter.api.AfterAll;
import org.junit.jupiter.api.BeforeAll;
import org.junit.jupiter.api.Test;

/** Runs the connector through the gRPC contract against an embedded Artemis broker (in-VM, no network). */
class JmsConnectorTest {
    private static final String URL = "vm://41";
    private static final String PASSWORD = "s3cr3t-pw";
    private static EmbeddedActiveMQ broker;
    private static ActiveMQJMSConnectionFactory client;

    @BeforeAll
    static void startBroker() throws Exception {
        Configuration cfg = new ConfigurationImpl()
                .setPersistenceEnabled(false)
                .setSecurityEnabled(false)
                .setJMXManagementEnabled(false)
                .addAcceptorConfiguration("invm", URL);
        broker = new EmbeddedActiveMQ().setConfiguration(cfg);
        broker.start();
        client = new ActiveMQJMSConnectionFactory(URL);
    }

    @AfterAll
    static void stopBroker() throws Exception {
        if (client != null) {
            client.close();
        }
        if (broker != null) {
            broker.stop();
        }
    }

    private static Map<String, Object> connection() {
        return Map.of("provider", "artemis", "brokerUrl", URL, "username", "u", "password", PASSWORD);
    }

    private static ExecuteStart start(String op, Map<String, Object> options) {
        return ConnectorTestHarness.start(
                        OperationType.OPERATION_TYPE_EXECUTE,
                        op,
                        ConnectorTestHarness.connection(connection()))
                .setOptions(Structs.fromMap(options))
                .build();
    }

    @Test
    void descriptorDeclaresTheThreeOperations() {
        try (ConnectorTestHarness h = ConnectorTestHarness.start(new JmsConnector())) {
            var d = h.describe();
            assertEquals("jms", d.getId());
            assertEquals(3, d.getOperationsCount());
            assertEquals("SEND", d.getOperations(0).getCustomType());
            assertTrue(d.getTrackedPropertiesList().contains("jms.messageId"));
        }
    }

    @Test
    void testConnectionReportsProviderWithoutSecrets() {
        try (ConnectorTestHarness h = ConnectorTestHarness.start(new JmsConnector())) {
            var r = h.blocking()
                    .test(TestRequest.newBuilder()
                            .setConnection(ConnectorTestHarness.connection(connection()))
                            .build());
            assertTrue(r.getOk(), r.getDiagnostic());
            assertTrue(r.getDiagnostic().contains("ActiveMQ"), r.getDiagnostic());
            assertFalse(r.getDiagnostic().contains(PASSWORD));
        }
    }

    @Test
    void sendToQueueDeliversTextPropertiesAndCorrelation() throws Exception {
        try (ConnectorTestHarness h = ConnectorTestHarness.start(new JmsConnector())) {
            Outcome o = h.execute(
                    start("SEND", Map.of("destination", "q.send")),
                    List.of(new Doc("d1", Map.of("order.id", "42", "jms.correlationId", "corr-1"), "hello jms".getBytes())),
                    4);
            assertTrue(o.status().isOk(), String.valueOf(o.status()));
            assertEquals(DocumentStatus.DOCUMENT_STATUS_SUCCESS, o.result("d1").getStatus());
            assertTrue(o.result("d1").getTrackedMap().get("jms.messageId").startsWith("ID:"));
            try (JMSContext c = client.createContext()) {
                Message m = c.createConsumer(c.createQueue("q.send")).receive(5000);
                assertNotNull(m);
                assertEquals("hello jms", ((TextMessage) m).getText());
                assertEquals("42", m.getStringProperty("order_id")); // '.' is not valid in a JMS property name
                assertEquals("corr-1", m.getJMSCorrelationID());
            }
        }
    }

    @Test
    void sendToTopicReachesSubscriber() throws Exception {
        try (JMSContext c = client.createContext();
                ConnectorTestHarness h = ConnectorTestHarness.start(new JmsConnector())) {
            JMSConsumer sub = c.createConsumer(c.createTopic("t.news"));
            Outcome o = h.execute(
                    start("SEND", Map.of("destination", "t.news", "destinationType", "topic", "messageType", "bytes")),
                    List.of(Doc.of("n1", "breaking")),
                    100);
            assertEquals(DocumentStatus.DOCUMENT_STATUS_SUCCESS, o.result("n1").getStatus());
            byte[] body = sub.receiveBody(byte[].class, 5000);
            assertEquals("breaking", new String(body));
        }
    }

    @Test
    void receiveEmitsOneOutputDocumentPerMessage() throws Exception {
        try (ConnectorTestHarness h = ConnectorTestHarness.start(new JmsConnector())) {
            h.execute(
                    start("SEND", Map.of("destination", "q.recv")),
                    List.of(Doc.of("s1", "first"), Doc.of("s2", "second"), Doc.of("s3", "third")),
                    100);
            Outcome o = h.execute(
                    start("RECEIVE", Map.of("destination", "q.recv", "maxMessages", 2, "timeoutMs", 3000)),
                    List.of(Doc.of("trigger", "")),
                    10);
            assertTrue(o.status().isOk(), String.valueOf(o.status()));
            assertEquals(DocumentStatus.DOCUMENT_STATUS_SUCCESS, o.result("trigger").getStatus());
            assertEquals("2", o.result("trigger").getTrackedMap().get("jms.received"));
            assertEquals(List.of("trigger-1", "trigger-2"), o.result("trigger").getOutputDocumentIdsList());
            assertEquals("first", o.outputs().get("trigger-1").text());
            assertEquals("second", o.outputs().get("trigger-2").text());
            assertEquals("text", o.outputs().get("trigger-1").properties().get("jms.bodyType"));
            assertEquals("q.recv", o.outputs().get("trigger-1").properties().get("jms.destination"));

            // The third message was not consumed by the first batch and is still available.
            Outcome rest = h.execute(
                    start("RECEIVE", Map.of("destination", "q.recv", "timeoutMs", 3000)),
                    List.of(Doc.of("again", "")),
                    10);
            assertEquals("third", rest.outputs().get("again-1").text());
        }
    }

    @Test
    void receiveFromEmptyQueueIsSuccessWithNoOutputs() throws Exception {
        try (ConnectorTestHarness h = ConnectorTestHarness.start(new JmsConnector())) {
            Outcome o = h.execute(
                    start("RECEIVE", Map.of("destination", "q.empty", "timeoutMs", 100)),
                    List.of(Doc.of("t", "")),
                    10);
            assertEquals(DocumentStatus.DOCUMENT_STATUS_SUCCESS, o.result("t").getStatus());
            assertEquals("0", o.result("t").getTrackedMap().get("jms.received"));
            assertTrue(o.outputs().isEmpty());
        }
    }

    @Test
    void receiveHonoursSelector() throws Exception {
        try (ConnectorTestHarness h = ConnectorTestHarness.start(new JmsConnector())) {
            h.execute(
                    start("SEND", Map.of("destination", "q.sel")),
                    List.of(
                            new Doc("a", Map.of("color", "red"), "red-one".getBytes()),
                            new Doc("b", Map.of("color", "blue"), "blue-one".getBytes())),
                    100);
            Outcome o = h.execute(
                    start("RECEIVE", Map.of("destination", "q.sel", "selector", "color = 'blue'", "timeoutMs", 3000)),
                    List.of(Doc.of("t", "")),
                    10);
            assertEquals("blue-one", o.outputs().get("t-1").text());
        }
    }

    @Test
    void requestReplyReturnsTheReplyAsOutputDocument() throws Exception {
        AtomicReference<Throwable> responderError = new AtomicReference<>();
        Thread responder = new Thread(() -> {
            try (JMSContext c = client.createContext()) {
                JMSConsumer in = c.createConsumer(c.createQueue("rr.in"));
                Message req = in.receive(15_000);
                if (req != null) {
                    TextMessage reply = c.createTextMessage(req.getBody(String.class).toUpperCase());
                    reply.setJMSCorrelationID(req.getJMSMessageID()); // the usual JMS convention
                    c.createProducer().send(req.getJMSReplyTo(), reply);
                }
            } catch (Exception e) {
                responderError.set(e);
            }
        });
        responder.start();
        try (ConnectorTestHarness h = ConnectorTestHarness.start(new JmsConnector())) {
            Outcome o = h.execute(
                    start("REQUEST_REPLY", Map.of("destination", "rr.in", "timeoutMs", 10_000)),
                    List.of(Doc.of("rq", "ping")),
                    100);
            assertTrue(o.status().isOk(), String.valueOf(o.status()));
            assertEquals(DocumentStatus.DOCUMENT_STATUS_SUCCESS, o.result("rq").getStatus(), o.result("rq").getMessage());
            assertEquals("PING", o.outputs().get("rq-reply").text());
        } finally {
            responder.join(20_000);
        }
        assertNull(responderError.get());
    }

    @Test
    void requestReplyTimeoutIsADocumentFailure() throws Exception {
        try (ConnectorTestHarness h = ConnectorTestHarness.start(new JmsConnector())) {
            Outcome o = h.execute(
                    start("REQUEST_REPLY", Map.of("destination", "rr.nobody", "timeoutMs", 300)),
                    List.of(Doc.of("rq", "anyone there?")),
                    100);
            assertEquals(DocumentStatus.DOCUMENT_STATUS_FAILURE, o.result("rq").getStatus());
            assertTrue(o.result("rq").getMessage().contains("no reply"));
        }
    }

    @Test
    void unreachableBrokerFailsEachDocumentWithoutLeakingSecrets() throws Exception {
        try (ConnectorTestHarness h = ConnectorTestHarness.start(new JmsConnector())) {
            ExecuteStart start = ConnectorTestHarness.start(
                            OperationType.OPERATION_TYPE_EXECUTE,
                            "SEND",
                            ConnectorTestHarness.connection(Map.of(
                                    "provider", "artemis",
                                    "brokerUrl", "tcp://127.0.0.1:1",
                                    "username", "u",
                                    "password", PASSWORD)))
                    .setOptions(Structs.of("destination", "q.x"))
                    .build();
            Outcome o = h.execute(start, List.of(Doc.of("a", "1"), Doc.of("b", "2")), 10);
            assertTrue(o.status().isOk(), String.valueOf(o.status()));
            assertEquals(DocumentStatus.DOCUMENT_STATUS_FAILURE, o.result("a").getStatus());
            assertEquals(DocumentStatus.DOCUMENT_STATUS_FAILURE, o.result("b").getStatus());
            assertFalse(o.result("a").getMessage().contains(PASSWORD));
        }
    }

    @Test
    void missingDestinationFailsTheCallAsInvalidArgument() throws Exception {
        try (ConnectorTestHarness h = ConnectorTestHarness.start(new JmsConnector())) {
            Outcome o = h.execute(start("SEND", Map.of()), List.of(Doc.of("a", "1")), 10);
            assertEquals(Status.Code.INVALID_ARGUMENT, o.status().getCode());
        }
    }

    @Test
    void ibmMqProviderWithoutJarExplainsThePluginDirectory() throws Exception {
        try (ConnectorTestHarness h = ConnectorTestHarness.start(new JmsConnector())) {
            ExecuteStart start = ConnectorTestHarness.start(
                            OperationType.OPERATION_TYPE_EXECUTE,
                            "SEND",
                            ConnectorTestHarness.connection(Map.of(
                                    "provider", "ibmmq",
                                    "factoryProperties", Map.of("hostName", "mq", "port", 1414, "queueManager", "QM1"))))
                    .setOptions(Structs.of("destination", "DEV.QUEUE.1"))
                    .build();
            Outcome o = h.execute(start, List.of(Doc.of("a", "1")), 10);
            assertEquals(Status.Code.FAILED_PRECONDITION, o.status().getCode());
            assertTrue(o.status().getDescription().contains("plugin directory"), o.status().getDescription());
        }
    }

    @Test
    void propertyNamesAreSanitizedToJmsIdentifiers() {
        assertEquals("order_id", JmsConnector.propertyName("order.id"));
        assertEquals("_1st", JmsConnector.propertyName("1st"));
        assertEquals("x_JMSType", JmsConnector.propertyName("JMSType"));
        assertEquals("x_not",JmsConnector.propertyName("not"));
    }
}

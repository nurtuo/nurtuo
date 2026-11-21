// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.jms;

import io.nurtuo.bridge.sdk.ConnectorException;
import io.nurtuo.bridge.sdk.Settings;
import jakarta.jms.DeliveryMode;
import jakarta.jms.Destination;
import jakarta.jms.JMSContext;
import java.util.Locale;

/** Validated operation options shared by SEND, RECEIVE and REQUEST_REPLY. */
final class JmsOptions {
    static final String SEND = "SEND";
    static final String RECEIVE = "RECEIVE";
    static final String REQUEST_REPLY = "REQUEST_REPLY";
    static final int MAX_MESSAGES_CAP = 1000;

    final String operation;
    final String destinationName;
    final boolean topic;
    final boolean textMessages;
    final int deliveryMode;
    final int priority;
    final long timeToLiveMs;
    final String correlationId;
    final int maxMessages;
    final long timeoutMs;
    final String selector;
    final String subscriptionName;
    final String replyQueue;

    private JmsOptions(String operation, Settings o) {
        this.operation = operation;
        this.destinationName = o.requireString("destination");
        String type = o.string("destinationType", "queue").toLowerCase(Locale.ROOT);
        if (!type.equals("queue") && !type.equals("topic")) {
            throw ConnectorException.invalidArgument("destinationType must be 'queue' or 'topic'");
        }
        this.topic = type.equals("topic");
        String mt = o.string("messageType", "text").toLowerCase(Locale.ROOT);
        if (!mt.equals("text") && !mt.equals("bytes")) {
            throw ConnectorException.invalidArgument("messageType must be 'text' or 'bytes'");
        }
        this.textMessages = mt.equals("text");
        String dm = o.string("deliveryMode", "persistent").toLowerCase(Locale.ROOT);
        if (dm.equals("persistent")) {
            this.deliveryMode = DeliveryMode.PERSISTENT;
        } else if (dm.equals("non_persistent")) {
            this.deliveryMode = DeliveryMode.NON_PERSISTENT;
        } else {
            throw ConnectorException.invalidArgument("deliveryMode must be 'persistent' or 'non_persistent'");
        }
        long prio = o.integer("priority", 4);
        if (prio < 0 || prio > 9) {
            throw ConnectorException.invalidArgument("priority must be 0..9");
        }
        this.priority = (int) prio;
        this.timeToLiveMs = Math.max(0, o.integer("timeToLiveMs", 0));
        this.correlationId = o.string("correlationId").orElse(null);
        long max = o.integer("maxMessages", 1);
        if (max < 1 || max > MAX_MESSAGES_CAP) {
            throw ConnectorException.invalidArgument("maxMessages must be 1.." + MAX_MESSAGES_CAP);
        }
        this.maxMessages = (int) max;
        this.timeoutMs = Math.max(0, o.integer("timeoutMs", operation.equals(REQUEST_REPLY) ? 30_000 : 1_000));
        this.selector = o.string("selector").filter(s -> !s.isBlank()).orElse(null);
        this.subscriptionName = o.string("subscriptionName").filter(s -> !s.isBlank()).orElse(null);
        this.replyQueue = o.string("replyQueue").filter(s -> !s.isBlank()).orElse(null);
        if (subscriptionName != null && !topic) {
            throw ConnectorException.invalidArgument("subscriptionName only applies to topics");
        }
    }

    static JmsOptions parse(String customType, Settings options) {
        switch (customType) {
            case SEND:
            case RECEIVE:
            case REQUEST_REPLY:
                return new JmsOptions(customType, options);
            default:
                throw ConnectorException.invalidArgument(
                        "unsupported operation '" + customType + "' (SEND, RECEIVE, REQUEST_REPLY)");
        }
    }

    Destination destination(JMSContext jms) {
        return topic ? jms.createTopic(destinationName) : jms.createQueue(destinationName);
    }
}

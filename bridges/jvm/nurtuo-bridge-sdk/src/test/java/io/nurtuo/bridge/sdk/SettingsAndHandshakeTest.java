// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.sdk;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.util.List;
import java.util.Map;
import java.util.Set;
import org.junit.jupiter.api.Test;

class SettingsAndHandshakeTest {
    private static Settings settings() {
        return new Settings(
                Structs.of(
                        "host", "mq.example.com",
                        "port", 1414,
                        "password", "p4ss",
                        "factory", Map.of("channel", "DEV.APP", "sslKeyPassphrase", "kp", "plain", "v"),
                        "custom", "c-secret"),
                Set.of("custom"));
    }

    @Test
    void typedAccessors() {
        Settings s = settings();
        assertEquals("mq.example.com", s.requireString("host"));
        assertEquals("1414", s.string("port").orElseThrow());
        assertEquals(1414, s.integer("port", 0));
        assertEquals(7, s.integer("missing", 7));
        assertEquals("DEV.APP", s.object("factory").requireString("channel"));
        assertThrows(ConnectorException.class, () -> s.requireString("nope"));
    }

    @Test
    void secretValuesAreCollectedAtAnyDepthAndMasked() {
        Settings s = settings();
        assertEquals(Set.of("p4ss", "kp", "c-secret"), s.secretValues());
        assertFalse(s.toString().contains("p4ss"));
        assertFalse(s.toString().contains("c-secret"));
        assertEquals("***", new Secret("x").toString());
    }

    @Test
    void redactorMasksLongestFirstAndThrowables() {
        Redactor r = new Redactor(List.of("abc", "abcdef"));
        assertEquals("*** and ***", r.redact("abcdef and abc"));
        assertEquals("IllegalStateException: no ***", r.redact(new IllegalStateException("no abc")));
    }

    @Test
    void handshakeLineRoundTrips() {
        ListenAddress tcp = ListenAddress.tcp("127.0.0.1", 43211);
        assertEquals("NURTUO-BRIDGE|1|tcp|127.0.0.1:43211|nurtuo.connector.v1|jvm", Handshake.line(tcp));
        Handshake.Info info = Handshake.parse(Handshake.line(tcp, "nurtuo.connector.v1", "jvm/0.1.0")).orElseThrow();
        assertEquals("tcp:127.0.0.1:43211", info.address().toString());
        assertEquals("nurtuo.connector.v1", info.contract());
        assertEquals("jvm/0.1.0", info.implementation());
        ListenAddress unix = ListenAddress.unix("/run/nurtuo/b.sock");
        assertEquals("NURTUO-BRIDGE|1|unix|/run/nurtuo/b.sock|nurtuo.connector.v1|jvm", Handshake.line(unix));
        assertEquals(ListenAddress.Kind.UNIX, Handshake.parse(Handshake.line(unix)).orElseThrow().address().kind());
        // the implementation field may contain '|'
        assertEquals("a|b", Handshake.parse("NURTUO-BRIDGE|1|tcp|127.0.0.1:1|nurtuo.connector.v1|a|b").orElseThrow().implementation());
        assertTrue(Handshake.parse("some log line").isEmpty());
        assertTrue(Handshake.parse("NURTUO-BRIDGE|2|tcp|127.0.0.1:1|nurtuo.connector.v1|x").isEmpty());
        assertTrue(Handshake.parse("NURTUO-BRIDGE|1|tcp|garbage|nurtuo.connector.v1|x").isEmpty());
    }
}

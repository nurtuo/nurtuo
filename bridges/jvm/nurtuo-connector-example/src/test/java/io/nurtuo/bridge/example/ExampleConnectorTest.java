// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.example;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import io.nurtuo.bridge.sdk.Structs;
import io.nurtuo.bridge.sdk.testing.ConnectorTestHarness;
import io.nurtuo.bridge.sdk.testing.ConnectorTestHarness.Doc;
import io.nurtuo.bridge.sdk.testing.ConnectorTestHarness.Outcome;
import java.util.List;
import java.util.Map;
import java.util.Random;
import nurtuo.connector.v1.Connector.BrowseRequest;
import nurtuo.connector.v1.Connector.DocumentStatus;
import nurtuo.connector.v1.Connector.ExecuteStart;
import nurtuo.connector.v1.Connector.Host;
import nurtuo.connector.v1.Connector.OperationType;
import nurtuo.connector.v1.Connector.TestRequest;
import org.junit.jupiter.api.Test;

class ExampleConnectorTest {
    private static final Map<String, Object> SETTINGS = Map.of("endpoint", "https://example.invalid", "apiKey", "k-12345");

    private static ExecuteStart start(String mode) {
        return ConnectorTestHarness.start(
                        OperationType.OPERATION_TYPE_EXECUTE, "TRANSFORM", ConnectorTestHarness.connection(SETTINGS))
                .setOptions(Structs.of("mode", mode))
                .build();
    }

    @Test
    void descriptorIsServedOverGrpc() {
        try (ConnectorTestHarness h = ConnectorTestHarness.start(new ExampleConnector())) {
            var d = h.describe();
            assertEquals("example-text", d.getId());
            assertEquals(Host.HOST_BRIDGE_JVM, d.getHost());
            assertEquals("TRANSFORM", d.getOperations(0).getCustomType());
            assertTrue(d.getOperations(0).getSupportsBrowse());
        }
    }

    @Test
    void transformsDocumentsAndReportsPerDocumentStatus() throws Exception {
        try (ConnectorTestHarness h = ConnectorTestHarness.start(new ExampleConnector())) {
            Outcome o = h.execute(
                    start("upper"),
                    List.of(
                            Doc.of("a", "hello world"),
                            Doc.of("b", "REJECT this one"),
                            Doc.of("c", ""),
                            Doc.of("d", "last")),
                    4);
            assertTrue(o.status().isOk(), String.valueOf(o.status()));
            assertEquals(DocumentStatus.DOCUMENT_STATUS_SUCCESS, o.result("a").getStatus());
            assertEquals("HELLO WORLD", o.outputs().get("a.out").text());
            assertEquals("11", o.result("a").getTrackedMap().get("example.bytes"));
            assertEquals(DocumentStatus.DOCUMENT_STATUS_APPLICATION_ERROR, o.result("b").getStatus());
            assertEquals(DocumentStatus.DOCUMENT_STATUS_APPLICATION_ERROR, o.result("c").getStatus());
            assertEquals(DocumentStatus.DOCUMENT_STATUS_SUCCESS, o.result("d").getStatus());
            assertEquals("LAST", o.outputs().get("d.out").text());
        }
    }

    @Test
    void reverseAndUnknownMode() throws Exception {
        try (ConnectorTestHarness h = ConnectorTestHarness.start(new ExampleConnector())) {
            Outcome o = h.execute(start("reverse"), List.of(Doc.of("a", "abcdef")), 100);
            assertEquals("fedcba", o.outputs().get("a.out").text());
            Outcome bad = h.execute(start("shout"), List.of(Doc.of("a", "abcdef")), 100);
            assertEquals(DocumentStatus.DOCUMENT_STATUS_APPLICATION_ERROR, bad.result("a").getStatus());
        }
    }

    @Test
    void streamsLargeDocumentsUnchangedInNoneMode() throws Exception {
        byte[] big = new byte[5 * 1024 * 1024 + 3];
        new Random(7).nextBytes(big);
        big[0] = 'x'; // not "REJECT"
        try (ConnectorTestHarness h = ConnectorTestHarness.start(new ExampleConnector())) {
            Outcome o = h.execute(start("none"), List.of(new Doc("big", Map.of(), big)), 32 * 1024);
            assertEquals(DocumentStatus.DOCUMENT_STATUS_SUCCESS, o.result("big").getStatus());
            assertArrayEquals(big, o.outputs().get("big.out").data());
            assertEquals(Long.toString(big.length), o.result("big").getTrackedMap().get("example.bytes"));
        }
    }

    @Test
    void testConnectionNeverEchoesTheApiKey() {
        try (ConnectorTestHarness h = ConnectorTestHarness.start(new ExampleConnector())) {
            var r = h.blocking()
                    .test(TestRequest.newBuilder()
                            .setConnection(ConnectorTestHarness.connection(SETTINGS))
                            .build());
            assertTrue(r.getOk());
            assertFalse(r.getDiagnostic().contains("k-12345"));
        }
    }

    @Test
    void browseListsObjectTypesAndProfiles() {
        try (ConnectorTestHarness h = ConnectorTestHarness.start(new ExampleConnector())) {
            var list = h.blocking().browse(BrowseRequest.getDefaultInstance());
            assertEquals("text", list.getObjectTypes(0).getId());
            var profile = h.blocking().browse(BrowseRequest.newBuilder().setObjectTypeId("text").build());
            assertEquals("{\"type\":\"string\"}", profile.getRequestProfile().getDefinition().toStringUtf8());
        }
    }
}

// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.sdk;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import io.grpc.Status;
import io.grpc.StatusRuntimeException;
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

class ExecuteContractTest {
    private static final String PASSWORD = "hunter2-s3cret";
    private static final Map<String, Object> SETTINGS = Map.of("user", "alice", "password", PASSWORD);

    private static ExecuteStart start(Map<String, Object> options) {
        return ConnectorTestHarness.start(
                        OperationType.OPERATION_TYPE_EXECUTE,
                        "ECHO",
                        ConnectorTestHarness.connection(SETTINGS))
                .setOptions(Structs.fromMap(options))
                .build();
    }

    private static Doc doc(String id, String kind, byte[] data) {
        return new Doc(id, Map.of("kind", kind), data);
    }

    @Test
    void describeDefaultsHostToBridgeJvm() {
        try (ConnectorTestHarness h = ConnectorTestHarness.start(new EchoConnector())) {
            assertEquals(Host.HOST_BRIDGE_JVM, h.describe().getHost());
            assertEquals("test-echo", h.describe().getId());
        }
    }

    @Test
    void perDocumentStatusesAreResultsNotGrpcErrors() throws Exception {
        try (ConnectorTestHarness h = ConnectorTestHarness.start(new EchoConnector())) {
            Outcome o = h.execute(
                    start(Map.of()),
                    List.of(
                            doc("ok", "echo", "hello".getBytes()),
                            doc("bad", "reject", "x".getBytes()),
                            doc("boom", "crash", "x".getBytes()),
                            doc("after", "echo", "still works".getBytes())),
                    3);
            assertTrue(o.status().isOk(), String.valueOf(o.status()));
            assertEquals(DocumentStatus.DOCUMENT_STATUS_SUCCESS, o.result("ok").getStatus());
            assertEquals(List.of("ok-out"), o.result("ok").getOutputDocumentIdsList());
            assertEquals("hello", o.outputs().get("ok-out").text());
            assertEquals("echo", o.outputs().get("ok-out").properties().get("kind"));
            assertEquals(DocumentStatus.DOCUMENT_STATUS_APPLICATION_ERROR, o.result("bad").getStatus());
            assertEquals(DocumentStatus.DOCUMENT_STATUS_FAILURE, o.result("boom").getStatus());
            assertEquals(DocumentStatus.DOCUMENT_STATUS_SUCCESS, o.result("after").getStatus());
            assertEquals("still works", o.outputs().get("after-out").text());
        }
    }

    @Test
    void secretsNeverAppearInResultMessages() throws Exception {
        try (ConnectorTestHarness h = ConnectorTestHarness.start(new EchoConnector())) {
            Outcome o = h.execute(start(Map.of()), List.of(doc("boom", "crash", new byte[0])), 8);
            String message = o.result("boom").getMessage();
            assertFalse(message.contains(PASSWORD), message);
            assertTrue(message.contains(Redactor.MASK), message);
        }
    }

    @Test
    void unreadPayloadIsSkippedAndNextDocumentIsIntact() throws Exception {
        byte[] big = new byte[200_000];
        new Random(1).nextBytes(big);
        try (ConnectorTestHarness h = ConnectorTestHarness.start(new EchoConnector())) {
            Outcome o = h.execute(
                    start(Map.of()), List.of(doc("skipped", "ignore", big), doc("next", "echo", "ok".getBytes())), 1000);
            assertEquals(DocumentStatus.DOCUMENT_STATUS_SUCCESS, o.result("skipped").getStatus());
            assertEquals("ok", o.outputs().get("next-out").text());
        }
    }

    @Test
    void largePayloadRoundTripsInChunks() throws Exception {
        byte[] big = new byte[3 * 1024 * 1024 + 17]; // not a multiple of the 64 KiB output chunk
        new Random(42).nextBytes(big);
        try (ConnectorTestHarness h = ConnectorTestHarness.start(new EchoConnector())) {
            Outcome o = h.execute(start(Map.of()), List.of(doc("big", "echo", big)), 10_000);
            assertEquals(DocumentStatus.DOCUMENT_STATUS_SUCCESS, o.result("big").getStatus());
            assertArrayEquals(big, o.outputs().get("big-out").data());
        }
    }

    @Test
    void emptyPayloadIsADocument() throws Exception {
        try (ConnectorTestHarness h = ConnectorTestHarness.start(new EchoConnector())) {
            Outcome o = h.execute(start(Map.of()), List.of(doc("empty", "echo", new byte[0])), 10);
            assertEquals(DocumentStatus.DOCUMENT_STATUS_SUCCESS, o.result("empty").getStatus());
            assertEquals(0, o.outputs().get("empty-out").data().length);
        }
    }

    @Test
    void connectorLevelErrorFailsTheCallWithGrpcStatus() throws Exception {
        try (ConnectorTestHarness h = ConnectorTestHarness.start(new EchoConnector())) {
            Outcome o = h.execute(start(Map.of("mode", "INVALID")), List.of(doc("a", "echo", "x".getBytes())), 8);
            assertEquals(Status.Code.INVALID_ARGUMENT, o.status().getCode());
        }
    }

    @Test
    void documentsTheConnectorNeverReportedBecomeFailure() throws Exception {
        Connector lazy = new EchoConnector() {
            @Override
            public void execute(ExecuteContext ctx, Documents docs, ResultSink sink) throws Exception {
                InputDocument first = docs.next();
                sink.success(first.id());
                // returns without touching the remaining documents
            }
        };
        try (ConnectorTestHarness h = ConnectorTestHarness.start(lazy)) {
            Outcome o = h.execute(
                    start(Map.of()),
                    List.of(doc("d1", "echo", "1".getBytes()), doc("d2", "echo", "2".getBytes())),
                    4);
            assertTrue(o.status().isOk(), String.valueOf(o.status()));
            assertEquals(DocumentStatus.DOCUMENT_STATUS_SUCCESS, o.result("d1").getStatus());
            assertEquals(DocumentStatus.DOCUMENT_STATUS_FAILURE, o.result("d2").getStatus());
        }
    }

    @Test
    void testConnectionDiagnosticIsRedacted() {
        try (ConnectorTestHarness h = ConnectorTestHarness.start(new EchoConnector())) {
            var response = h.blocking()
                    .test(TestRequest.newBuilder()
                            .setConnection(ConnectorTestHarness.connection(SETTINGS))
                            .build());
            assertTrue(response.getOk());
            assertTrue(response.getDiagnostic().contains("alice"));
            assertFalse(response.getDiagnostic().contains(PASSWORD), response.getDiagnostic());
        }
    }

    @Test
    void testConnectionExceptionBecomesNotOkWithRedactedMessage() {
        Connector failing = new EchoConnector() {
            @Override
            public TestResult test(Settings connection) throws Exception {
                throw new java.io.IOException("auth failed using " + connection.secret("password").get().reveal());
            }
        };
        try (ConnectorTestHarness h = ConnectorTestHarness.start(failing)) {
            var response = h.blocking()
                    .test(TestRequest.newBuilder()
                            .setConnection(ConnectorTestHarness.connection(SETTINGS))
                            .build());
            assertFalse(response.getOk());
            assertFalse(response.getDiagnostic().contains(PASSWORD), response.getDiagnostic());
            assertNotEquals("", response.getDiagnostic());
        }
    }

    @Test
    void optionalRpcsAreUnimplemented() {
        try (ConnectorTestHarness h = ConnectorTestHarness.start(new EchoConnector())) {
            StatusRuntimeException e = assertThrows(
                    StatusRuntimeException.class,
                    () -> h.blocking().browse(BrowseRequest.getDefaultInstance()));
            assertEquals(Status.Code.UNIMPLEMENTED, e.getStatus().getCode());
        }
    }
}

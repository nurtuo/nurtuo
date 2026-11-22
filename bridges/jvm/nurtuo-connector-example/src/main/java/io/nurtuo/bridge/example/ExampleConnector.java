// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.example;

import com.google.protobuf.Struct;
import io.nurtuo.bridge.sdk.ApplicationException;
import io.nurtuo.bridge.sdk.Connector;
import io.nurtuo.bridge.sdk.ExecuteContext;
import io.nurtuo.bridge.sdk.InputDocument;
import io.nurtuo.bridge.sdk.ResultSink;
import io.nurtuo.bridge.sdk.Settings;
import io.nurtuo.bridge.sdk.Structs;
import io.nurtuo.bridge.sdk.TestResult;
import java.io.FilterInputStream;
import java.io.IOException;
import java.io.InputStream;
import java.nio.charset.StandardCharsets;
import java.util.List;
import java.util.Map;
import nurtuo.connector.v1.Connector.BrowseRequest;
import nurtuo.connector.v1.Connector.BrowseResponse;
import nurtuo.connector.v1.Connector.Capabilities;
import nurtuo.connector.v1.Connector.ConnectorDescriptor;
import nurtuo.connector.v1.Connector.Host;
import nurtuo.connector.v1.Connector.ObjectType;
import nurtuo.connector.v1.Connector.OperationDescriptor;
import nurtuo.connector.v1.Connector.OperationType;
import nurtuo.connector.v1.Connector.Profile;

/**
 * Reference connector that shows the whole authoring surface in about 100 lines.
 *
 * <p>Operation {@code EXECUTE/TRANSFORM}: copies each document to an output document, applying
 * {@code mode} (upper, lower, reverse, none). Upper/lower/none are streamed byte by byte, so
 * payloads of any size work; reverse needs the whole document. A document starting with
 * {@code REJECT} is a business error (APPLICATION_ERROR); empty documents are rejected too.
 * Connection settings: {@code endpoint} (any text) and {@code apiKey} (a secret, never echoed).
 */
public final class ExampleConnector extends Connector {
    public static final String ID = "example-text";

    @Override
    public ConnectorDescriptor describe() {
        Struct connection = Structs.of(
                "type", "object",
                "required", List.of("endpoint"),
                "properties", Map.of(
                        "endpoint", Map.of("type", "string"),
                        "apiKey", Map.of("type", "string", "format", "secret")));
        Struct options = Structs.of(
                "type", "object",
                "properties", Map.of("mode", Map.of("type", "string", "enum", List.of("upper", "lower", "reverse", "none"), "default", "none")));
        return ConnectorDescriptor.newBuilder()
                .setId(ID)
                .setVersion("0.1.0")
                .setHost(Host.HOST_BRIDGE_JVM)
                .setDisplayName("Example text connector")
                .setConnectionSchema(connection)
                .addOperations(OperationDescriptor.newBuilder()
                        .setType(OperationType.OPERATION_TYPE_EXECUTE)
                        .setCustomType("TRANSFORM")
                        .setSupportsBrowse(true)
                        .setOptionsSchema(options))
                .addTrackedProperties("example.bytes")
                .setCapabilities(Capabilities.newBuilder().setStreaming(true).setMaxBatchSize(1000))
                .build();
    }

    @Override
    public TestResult test(Settings connection) {
        String endpoint = connection.string("endpoint", "");
        if (endpoint.isBlank()) {
            return TestResult.failed("endpoint is not configured");
        }
        // Never put the key in the diagnostic; the SDK would mask it, but do not rely on that.
        return TestResult.ok("endpoint " + endpoint + " reachable, api key " + (connection.has("apiKey") ? "present" : "absent"));
    }

    @Override
    protected void executeDocument(ExecuteContext ctx, InputDocument doc, ResultSink sink) throws Exception {
        String mode = ctx.options().string("mode", "none");
        InputStream in = doc.data();
        byte[] head = in.readNBytes(6);
        if (head.length == 0) {
            throw new ApplicationException("empty document");
        }
        if (new String(head, StandardCharsets.UTF_8).equals("REJECT")) {
            throw new ApplicationException("document rejected by the example target");
        }
        InputStream whole = new java.io.SequenceInputStream(new java.io.ByteArrayInputStream(head), in);
        InputStream out;
        switch (mode) {
            case "upper":
                out = new MapStream(whole, true);
                break;
            case "lower":
                out = new MapStream(whole, false);
                break;
            case "reverse":
                byte[] all = whole.readAllBytes();
                StringBuilder sb = new StringBuilder(new String(all, StandardCharsets.UTF_8)).reverse();
                out = new java.io.ByteArrayInputStream(sb.toString().getBytes(StandardCharsets.UTF_8));
                break;
            case "none":
                out = whole;
                break;
            default:
                throw new ApplicationException("unknown mode '" + mode + "'");
        }
        CountingStream counted = new CountingStream(out);
        String outId = doc.id() + ".out";
        sink.output(outId, Map.of("mode", mode), counted);
        sink.success(doc.id(), Map.of("example.bytes", Long.toString(counted.count)), List.of(outId));
    }

    @Override
    public BrowseResponse browse(Settings connection, BrowseRequest request) {
        if (request.getObjectTypeId().isEmpty()) {
            return BrowseResponse.newBuilder()
                    .addObjectTypes(ObjectType.newBuilder().setId("text").setLabel("Plain text"))
                    .build();
        }
        String schema = "{\"type\":\"string\"}";
        Profile p = Profile.newBuilder()
                .setFormat(Profile.Format.FORMAT_JSON_SCHEMA)
                .setDefinition(com.google.protobuf.ByteString.copyFromUtf8(schema))
                .build();
        return BrowseResponse.newBuilder().setRequestProfile(p).setResponseProfile(p).build();
    }

    private static final class MapStream extends FilterInputStream {
        private final boolean upper;

        MapStream(InputStream in, boolean upper) {
            super(in);
            this.upper = upper;
        }

        private int map(int b) {
            if (b < 0 || b > 127) {
                return b; // leave multi-byte UTF-8 sequences untouched
            }
            return upper ? Character.toUpperCase(b) : Character.toLowerCase(b);
        }

        @Override
        public int read() throws IOException {
            return map(super.read());
        }

        @Override
        public int read(byte[] b, int off, int len) throws IOException {
            int n = super.read(b, off, len);
            for (int i = 0; i < n; i++) {
                b[off + i] = (byte) map(b[off + i] & 0xff);
            }
            return n;
        }
    }

    private static final class CountingStream extends FilterInputStream {
        long count;

        CountingStream(InputStream in) {
            super(in);
        }

        @Override
        public int read() throws IOException {
            int b = super.read();
            if (b >= 0) {
                count++;
            }
            return b;
        }

        @Override
        public int read(byte[] b, int off, int len) throws IOException {
            int n = super.read(b, off, len);
            if (n > 0) {
                count += n;
            }
            return n;
        }
    }
}

// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.sdk.testing;

import com.google.protobuf.ByteString;
import io.grpc.ManagedChannel;
import io.grpc.Server;
import io.grpc.Status;
import io.grpc.inprocess.InProcessChannelBuilder;
import io.grpc.inprocess.InProcessServerBuilder;
import io.grpc.stub.StreamObserver;
import io.nurtuo.bridge.sdk.Connector;
import io.nurtuo.bridge.sdk.ConnectorService;
import io.nurtuo.bridge.sdk.Structs;
import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.UUID;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import nurtuo.connector.v1.Connector.ConnectionConfig;
import nurtuo.connector.v1.Connector.DocumentChunk;
import nurtuo.connector.v1.Connector.DocumentResult;
import nurtuo.connector.v1.Connector.Descriptor;
import nurtuo.connector.v1.Connector.ExecuteEnd;
import nurtuo.connector.v1.Connector.ExecuteRequest;
import nurtuo.connector.v1.Connector.ExecuteResponse;
import nurtuo.connector.v1.Connector.ExecuteStart;
import nurtuo.connector.v1.ConnectorServiceGrpc;

/**
 * Serves a {@link Connector} over an in-process gRPC channel and offers a small client, so
 * connector authors can test through the real wire contract (chunking, per-document results,
 * status mapping) without sockets. Use with try-with-resources.
 */
public final class ConnectorTestHarness implements AutoCloseable {
    /** One input document of a batch. */
    public record Doc(String id, Map<String, String> properties, byte[] data) {
        public static Doc of(String id, String text) {
            return new Doc(id, Map.of(), text.getBytes(java.nio.charset.StandardCharsets.UTF_8));
        }
    }

    /** An output document reassembled from its chunks. */
    public record OutputDoc(String id, Map<String, String> properties, byte[] data) {
        public String text() {
            return new String(data, java.nio.charset.StandardCharsets.UTF_8);
        }
    }

    /** Everything the server sent for one Execute call. */
    public record Outcome(Map<String, DocumentResult> results, Map<String, OutputDoc> outputs, Status status) {
        public DocumentResult result(String documentId) {
            DocumentResult r = results.get(documentId);
            if (r == null) {
                throw new AssertionError("no result for document " + documentId + "; status=" + status);
            }
            return r;
        }
    }

    private final String name = "bridge-test-" + UUID.randomUUID();
    private final ConnectorService service;
    private final Server server;
    private final ManagedChannel channel;
    private final ConnectorServiceGrpc.ConnectorServiceBlockingStub blocking;
    private final ConnectorServiceGrpc.ConnectorServiceStub async;

    private ConnectorTestHarness(Connector connector) throws IOException {
        service = new ConnectorService(connector);
        server = InProcessServerBuilder.forName(name).addService(service).build().start();
        channel = InProcessChannelBuilder.forName(name).build();
        blocking = ConnectorServiceGrpc.newBlockingStub(channel);
        async = ConnectorServiceGrpc.newStub(channel);
    }

    public static ConnectorTestHarness start(Connector connector) {
        try {
            return new ConnectorTestHarness(connector);
        } catch (IOException e) {
            throw new IllegalStateException(e);
        }
    }

    public ManagedChannel channel() {
        return channel;
    }

    public ConnectorServiceGrpc.ConnectorServiceBlockingStub blocking() {
        return blocking;
    }

    public Descriptor describe() {
        return blocking.describe(nurtuo.connector.v1.Connector.DescribeRequest.getDefaultInstance()).getDescriptor();
    }

    public static ConnectionConfig connection(Map<String, ?> settings) {
        return ConnectionConfig.newBuilder().setSettings(Structs.fromMap(settings)).build();
    }

    /** Run one Execute batch: ExecuteStart, documents in chunks of {@code chunkSize} bytes, ExecuteEnd. */
    public Outcome execute(ExecuteStart start, List<Doc> docs, int chunkSize) throws InterruptedException {
        Map<String, DocumentResult> results = new LinkedHashMap<>();
        Map<String, OutputDoc> outputs = new LinkedHashMap<>();
        Map<String, Map<String, String>> outProps = new LinkedHashMap<>();
        Map<String, ByteArrayOutputStream> outData = new LinkedHashMap<>();
        CountDownLatch done = new CountDownLatch(1);
        Status[] status = {Status.OK};

        StreamObserver<ExecuteRequest> req = async.execute(new StreamObserver<>() {
            @Override
            public void onNext(ExecuteResponse r) {
                synchronized (results) {
                    switch (r.getMsgCase()) {
                        case RESULT:
                            results.put(r.getResult().getDocumentId(), r.getResult());
                            break;
                        case OUTPUT:
                            DocumentChunk c = r.getOutput();
                            outProps.computeIfAbsent(c.getDocumentId(), k -> new LinkedHashMap<>())
                                    .putAll(c.getPropertiesMap());
                            outData.computeIfAbsent(c.getDocumentId(), k -> new ByteArrayOutputStream())
                                    .writeBytes(c.getData().toByteArray());
                            if (c.getLast()) {
                                outputs.put(
                                        c.getDocumentId(),
                                        new OutputDoc(
                                                c.getDocumentId(),
                                                outProps.get(c.getDocumentId()),
                                                outData.get(c.getDocumentId()).toByteArray()));
                            }
                            break;
                        default:
                            break;
                    }
                }
            }

            @Override
            public void onError(Throwable t) {
                status[0] = Status.fromThrowable(t);
                done.countDown();
            }

            @Override
            public void onCompleted() {
                done.countDown();
            }
        });

        req.onNext(ExecuteRequest.newBuilder().setStart(start).build());
        for (Doc d : docs) {
            sendDocument(req, d, chunkSize);
        }
        req.onNext(ExecuteRequest.newBuilder().setEnd(ExecuteEnd.getDefaultInstance()).build());
        req.onCompleted();
        if (!done.await(30, TimeUnit.SECONDS)) {
            throw new AssertionError("Execute did not complete within 30s");
        }
        synchronized (results) {
            return new Outcome(new LinkedHashMap<>(results), new LinkedHashMap<>(outputs), status[0]);
        }
    }

    public static ExecuteStart.Builder start(
            nurtuo.connector.v1.Connector.OperationType type, String customType, ConnectionConfig connection) {
        return ExecuteStart.newBuilder()
                .setExecutionId("exec-1")
                .setStepId("step-1")
                .setOperationType(type)
                .setCustomType(customType)
                .setConnection(connection);
    }

    private static void sendDocument(StreamObserver<ExecuteRequest> req, Doc d, int chunkSize) {
        int size = Math.max(1, chunkSize);
        List<byte[]> parts = new ArrayList<>();
        for (int off = 0; off < d.data().length; off += size) {
            int end = Math.min(d.data().length, off + size);
            parts.add(java.util.Arrays.copyOfRange(d.data(), off, end));
        }
        if (parts.isEmpty()) {
            parts.add(new byte[0]);
        }
        for (int i = 0; i < parts.size(); i++) {
            DocumentChunk.Builder c = DocumentChunk.newBuilder()
                    .setDocumentId(d.id())
                    .setData(ByteString.copyFrom(parts.get(i)))
                    .setLast(i == parts.size() - 1);
            if (i == 0) {
                c.putAllProperties(d.properties());
            }
            req.onNext(ExecuteRequest.newBuilder().setChunk(c).build());
        }
    }

    @Override
    public void close() {
        channel.shutdownNow();
        server.shutdownNow();
        service.close();
        try {
            server.awaitTermination(5, TimeUnit.SECONDS);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
        }
    }
}

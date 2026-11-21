// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.sdk;

import io.grpc.Status;
import io.grpc.stub.ServerCallStreamObserver;
import io.grpc.stub.StreamObserver;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.logging.Level;
import java.util.logging.Logger;
import nurtuo.connector.v1.Connector.BrowseRequest;
import nurtuo.connector.v1.Connector.BrowseResponse;
import nurtuo.connector.v1.Connector.DescribeRequest;
import nurtuo.connector.v1.Connector.DescribeResponse;
import nurtuo.connector.v1.Connector.Descriptor;
import nurtuo.connector.v1.Connector.ExecuteRequest;
import nurtuo.connector.v1.Connector.ExecuteResponse;
import nurtuo.connector.v1.Connector.Host;
import nurtuo.connector.v1.Connector.TestRequest;
import nurtuo.connector.v1.Connector.TestResponse;
import nurtuo.connector.v1.ConnectorServiceGrpc;

/**
 * Adapts a {@link Connector} to the generated gRPC service. Listen is not implemented by the SDK
 * yet and answers UNIMPLEMENTED, which the contract allows.
 */
public final class ConnectorService extends ConnectorServiceGrpc.ConnectorServiceImplBase implements AutoCloseable {
    private static final Logger LOG = Logger.getLogger(ConnectorService.class.getName());

    private final Connector connector;
    private final ExecutorService executor;

    public ConnectorService(Connector connector) {
        this.connector = connector;
        AtomicInteger n = new AtomicInteger();
        // Worker threads must see the plugin classloader (customer jars) that BridgeMain installed
        // as the context classloader of the thread constructing this service.
        ClassLoader contextLoader = Thread.currentThread().getContextClassLoader();
        this.executor = Executors.newCachedThreadPool(r -> {
            Thread t = new Thread(r, "nurtuo-execute-" + n.incrementAndGet());
            t.setDaemon(true);
            t.setContextClassLoader(contextLoader);
            return t;
        });
    }

    public Connector connector() {
        return connector;
    }

    @Override
    public void describe(DescribeRequest request, StreamObserver<DescribeResponse> out) {
        try {
            Descriptor d = connector.describe();
            if (d.getHost() == Host.HOST_UNSPECIFIED) {
                d = d.toBuilder().setHost(Host.HOST_BRIDGE_JVM).build();
            }
            out.onNext(DescribeResponse.newBuilder().setDescriptor(d).build());
            out.onCompleted();
        } catch (RuntimeException e) {
            out.onError(toStatus(e, Redactor.none()).asRuntimeException());
        }
    }

    @Override
    public void browse(BrowseRequest request, StreamObserver<BrowseResponse> out) {
        Settings connection = new Settings(request.getConnection().getSettings(), connector.secretKeys());
        Redactor redactor = new Redactor(connection.secretValues());
        try {
            out.onNext(connector.browse(connection, request));
            out.onCompleted();
        } catch (Exception e) {
            out.onError(toStatus(e, redactor).asRuntimeException());
        }
    }

    @Override
    public void test(TestRequest request, StreamObserver<TestResponse> out) {
        Settings connection = new Settings(request.getConnection().getSettings(), connector.secretKeys());
        Redactor redactor = new Redactor(connection.secretValues());
        TestResponse response;
        try {
            TestResult r = connector.test(connection);
            response = TestResponse.newBuilder()
                    .setOk(r.ok())
                    .setDiagnostic(nullToEmpty(redactor.redact(r.diagnostic())))
                    .build();
        } catch (ConnectorException e) {
            if (e.kind() == ConnectorException.Kind.UNIMPLEMENTED) {
                out.onError(toStatus(e, redactor).asRuntimeException());
                return;
            }
            response = TestResponse.newBuilder()
                    .setOk(false)
                    .setDiagnostic(redactor.redact(e.getMessage()))
                    .build();
        } catch (Exception e) {
            LOG.log(Level.FINE, "test connection failed", e);
            response = TestResponse.newBuilder()
                    .setOk(false)
                    .setDiagnostic(redactor.redact(e))
                    .build();
        }
        out.onNext(response);
        out.onCompleted();
    }

    @Override
    public StreamObserver<ExecuteRequest> execute(StreamObserver<ExecuteResponse> responseObserver) {
        return new ExecuteSession(connector, (ServerCallStreamObserver<ExecuteResponse>) responseObserver, executor);
    }

    private static Status toStatus(Throwable t, Redactor redactor) {
        if (t instanceof ConnectorException) {
            return Status.fromCode(((ConnectorException) t).kind().code()).withDescription(redactor.redact(t.getMessage()));
        }
        LOG.log(Level.WARNING, "connector call failed", t);
        return Status.INTERNAL.withDescription(redactor.redact(t));
    }

    private static String nullToEmpty(String s) {
        return s == null ? "" : s;
    }

    @Override
    public void close() {
        executor.shutdownNow();
        try {
            executor.awaitTermination(2, TimeUnit.SECONDS);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
        }
        try {
            connector.close();
        } catch (RuntimeException e) {
            LOG.log(Level.WARNING, "connector close failed", e);
        }
    }
}

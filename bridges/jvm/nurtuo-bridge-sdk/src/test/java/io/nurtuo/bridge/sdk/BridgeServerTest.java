// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.sdk;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import io.grpc.ManagedChannel;
import io.grpc.ManagedChannelBuilder;
import io.grpc.Metadata;
import io.grpc.Status;
import io.grpc.StatusRuntimeException;
import io.grpc.health.v1.HealthCheckRequest;
import io.grpc.health.v1.HealthCheckResponse.ServingStatus;
import io.grpc.health.v1.HealthGrpc;
import io.grpc.stub.MetadataUtils;
import java.time.Duration;
import java.util.concurrent.TimeUnit;
import nurtuo.connector.v1.Connector.DescribeRequest;
import nurtuo.connector.v1.ConnectorServiceGrpc;
import org.junit.jupiter.api.Test;

/** Real TCP loopback server: handshake address, health, token auth, graceful shutdown. */
class BridgeServerTest {

    private static ManagedChannel channel(BridgeServer server) {
        ListenAddress a = server.address();
        return ManagedChannelBuilder.forAddress(a.host(), a.port()).usePlaintext().build();
    }

    @Test
    void servesDescribeAndHealthOnAnEphemeralPort() throws Exception {
        try (BridgeServer server = BridgeServer.builder(new EchoConnector())
                .listen(ListenAddress.tcp("127.0.0.1", 0))
                .build()
                .start()) {
            assertTrue(server.address().port() > 0);
            assertTrue(Handshake.line(server.address()).startsWith("NURTUO-BRIDGE|1|tcp|127.0.0.1:"));
            ManagedChannel ch = channel(server);
            try {
                var d = ConnectorServiceGrpc.newBlockingStub(ch)
                        .describe(DescribeRequest.getDefaultInstance())
                        .getDescriptor();
                assertEquals("test-echo", d.getId());
                var h = HealthGrpc.newBlockingStub(ch).check(HealthCheckRequest.getDefaultInstance());
                assertEquals(ServingStatus.SERVING, h.getStatus());
            } finally {
                ch.shutdownNow();
            }
        }
    }

    @Test
    void tokenIsRequiredExceptForHealth() throws Exception {
        try (BridgeServer server = BridgeServer.builder(new EchoConnector())
                .authToken("t0ken")
                .build()
                .start()) {
            ManagedChannel ch = channel(server);
            try {
                StatusRuntimeException e = assertThrows(
                        StatusRuntimeException.class,
                        () -> ConnectorServiceGrpc.newBlockingStub(ch).describe(DescribeRequest.getDefaultInstance()));
                assertEquals(Status.Code.UNAUTHENTICATED, e.getStatus().getCode());

                Metadata md = new Metadata();
                md.put(AuthInterceptor.TOKEN_HEADER, "t0ken");
                var ok = ConnectorServiceGrpc.newBlockingStub(ch)
                        .withInterceptors(MetadataUtils.newAttachHeadersInterceptor(md))
                        .describe(DescribeRequest.getDefaultInstance());
                assertEquals("test-echo", ok.getDescriptor().getId());

                assertEquals(
                        ServingStatus.SERVING,
                        HealthGrpc.newBlockingStub(ch).check(HealthCheckRequest.getDefaultInstance()).getStatus());
            } finally {
                ch.shutdownNow();
            }
        }
    }

    @Test
    void shutdownIsGracefulIdempotentAndStopsServing() throws Exception {
        BridgeServer server = BridgeServer.builder(new EchoConnector())
                .gracePeriod(Duration.ofSeconds(2))
                .build()
                .start();
        ManagedChannel ch = channel(server);
        try {
            server.shutdown();
            server.shutdown(); // idempotent
            server.awaitTermination();
            assertThrows(
                    StatusRuntimeException.class,
                    () -> ConnectorServiceGrpc.newBlockingStub(ch)
                            .withDeadlineAfter(2, TimeUnit.SECONDS)
                            .describe(DescribeRequest.getDefaultInstance()));
        } finally {
            ch.shutdownNow();
        }
    }
}

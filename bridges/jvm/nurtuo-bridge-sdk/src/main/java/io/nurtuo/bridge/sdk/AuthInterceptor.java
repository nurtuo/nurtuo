// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.sdk;

import io.grpc.Metadata;
import io.grpc.ServerCall;
import io.grpc.ServerCallHandler;
import io.grpc.ServerInterceptor;
import io.grpc.Status;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;

/**
 * Optional shared-secret check for TCP listeners (a loopback port is reachable by every local
 * user). The runtime generates a random token per bridge process, passes it in the
 * {@code NURTUO_BRIDGE_TOKEN} environment variable (never on the command line) and sends it as the
 * {@code x-nurtuo-bridge-token} metadata header. The standard health service is exempt so that
 * supervisors can probe liveness without the token.
 */
final class AuthInterceptor implements ServerInterceptor {
    static final Metadata.Key<String> TOKEN_HEADER =
            Metadata.Key.of("x-nurtuo-bridge-token", Metadata.ASCII_STRING_MARSHALLER);

    private final byte[] token;

    AuthInterceptor(String token) {
        this.token = token.getBytes(StandardCharsets.UTF_8);
    }

    @Override
    public <ReqT, RespT> ServerCall.Listener<ReqT> interceptCall(
            ServerCall<ReqT, RespT> call, Metadata headers, ServerCallHandler<ReqT, RespT> next) {
        if (call.getMethodDescriptor().getFullMethodName().startsWith("grpc.health.v1.Health/")) {
            return next.startCall(call, headers);
        }
        String presented = headers.get(TOKEN_HEADER);
        if (presented == null
                || !MessageDigest.isEqual(token, presented.getBytes(StandardCharsets.UTF_8))) {
            call.close(Status.UNAUTHENTICATED.withDescription("missing or invalid bridge token"), new Metadata());
            return new ServerCall.Listener<ReqT>() {};
        }
        return next.startCall(call, headers);
    }
}

// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.sdk;

import java.io.InputStream;
import java.util.List;
import java.util.Map;
import nurtuo.connector.v1.Connector.Capabilities;
import nurtuo.connector.v1.Connector.ConnectorDescriptor;
import nurtuo.connector.v1.Connector.OperationDescriptor;
import nurtuo.connector.v1.Connector.OperationType;

/** Test connector exercising every result path of the default per-document execute loop. */
class EchoConnector extends Connector {
    @Override
    public ConnectorDescriptor describe() {
        return ConnectorDescriptor.newBuilder()
                .setId("test-echo")
                .setVersion("1.0.0")
                .setDisplayName("Test echo")
                .addOperations(OperationDescriptor.newBuilder().setType(OperationType.OPERATION_TYPE_EXECUTE).setCustomType("ECHO"))
                .setCapabilities(Capabilities.newBuilder().setStreaming(true).setMaxBatchSize(100))
                .build();
    }

    @Override
    public TestResult test(Settings connection) {
        // Deliberately leaks the password: the SDK must redact it.
        return TestResult.ok("connected as " + connection.string("user", "?") + " with password "
                + connection.secret("password").map(Secret::reveal).orElse(""));
    }

    @Override
    protected void executeDocument(ExecuteContext ctx, InputDocument doc, ResultSink sink) throws Exception {
        if ("INVALID".equals(ctx.options().string("mode", ""))) {
            throw ConnectorException.invalidArgument("mode INVALID");
        }
        String kind = doc.properties().getOrDefault("kind", "echo");
        switch (kind) {
            case "reject":
                throw new ApplicationException("rejected by target: " + doc.id());
            case "crash":
                throw new IllegalStateException("login failed for password "
                        + ctx.connection().secret("password").map(Secret::reveal).orElse(""));
            case "ignore":
                // Not reading the payload at all must still work and keep later documents intact.
                sink.success(doc.id());
                return;
            default:
                try (InputStream in = doc.data()) {
                    sink.output(doc.id() + "-out", Map.of("kind", "echo", "source", doc.id()), in);
                }
                sink.success(doc.id(), Map.of("size", "n/a"), List.of(doc.id() + "-out"));
        }
    }
}

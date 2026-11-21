// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.sdk;

import java.util.Set;
import nurtuo.connector.v1.Connector.BrowseRequest;
import nurtuo.connector.v1.Connector.BrowseResponse;
import nurtuo.connector.v1.Connector.Descriptor;

/**
 * Base class for connectors hosted by the JVM bridge: describe / test / execute / browse.
 *
 * <p>Implementations need a public no-argument constructor and are registered with
 * {@code META-INF/services/io.nurtuo.bridge.sdk.Connector} (ServiceLoader) so the launcher can find
 * them. One instance serves all calls concurrently, so keep instance state thread-safe and open
 * target-system connections per call (or pool them yourself).
 *
 * <p>Error model: throw {@link ApplicationException} for a business rejection of one document,
 * any other exception for a technical failure of one document, and {@link ConnectorException} for
 * problems that fail the whole call (bad settings, unsupported operation).
 */
public abstract class Connector implements AutoCloseable {

    /** The descriptor (id, SemVer version, operations, connection schema, capabilities). */
    public abstract Descriptor describe();

    /**
     * Verify the connection. Return a diagnostic without secrets (the SDK also redacts known
     * secret values). Exceptions are turned into {@code ok=false} with a redacted message.
     */
    public abstract TestResult test(Settings connection) throws Exception;

    /**
     * Process a batch. The default implementation calls {@link #executeDocument} for each document
     * and maps exceptions to per-document statuses; override for batch-oriented connectors.
     */
    public void execute(ExecuteContext ctx, Documents docs, ResultSink sink) throws Exception {
        InputDocument doc;
        while ((doc = docs.next()) != null) {
            if (ctx.isCancelled()) {
                return;
            }
            try {
                executeDocument(ctx, doc, sink);
                if (!sink.hasResult(doc.id())) {
                    sink.success(doc.id());
                }
            } catch (ApplicationException e) {
                sink.applicationError(doc.id(), e.getMessage());
            } catch (ConnectorException | InterruptedException e) {
                throw e;
            } catch (Exception e) {
                sink.failure(doc.id(), ctx.redactor().redact(e));
            }
        }
    }

    /**
     * Process one document. Report the result on {@code sink}, or return normally for SUCCESS.
     */
    protected void executeDocument(ExecuteContext ctx, InputDocument doc, ResultSink sink) throws Exception {
        throw ConnectorException.unimplemented("connector does not implement executeDocument");
    }

    /** Browse object types / profiles. Default: UNIMPLEMENTED. */
    public BrowseResponse browse(Settings connection, BrowseRequest request) throws Exception {
        throw ConnectorException.unimplemented("browse is not supported by this connector");
    }

    /**
     * Setting keys (besides those matching {@link Settings#SECRET_KEY}) whose values are secrets.
     */
    public Set<String> secretKeys() {
        return Set.of();
    }

    /** Called once on shutdown; release pooled resources. */
    @Override
    public void close() {}
}

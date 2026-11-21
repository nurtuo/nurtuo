// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.sdk;

import java.io.IOException;
import java.io.InputStream;
import java.util.List;
import java.util.Map;

/**
 * The output side of an Execute batch. Exactly one result must be reported per input document;
 * the SDK reports FAILURE for documents that end the batch without one. Messages are redacted
 * (known secret values replaced by ***) and truncated before they are sent.
 *
 * <p>Implementations are thread-safe.
 */
public interface ResultSink {
    void success(String documentId);

    /**
     * @param tracked tracked-property values for Process Reporting (keys declared in the
     *     descriptor's tracked_properties)
     * @param outputDocumentIds ids of output documents emitted for this input
     */
    void success(String documentId, Map<String, String> tracked, List<String> outputDocumentIds);

    /** The target system rejected the document (business error). */
    void applicationError(String documentId, String message);

    /** Technical failure for this document (connection, timeout, crash). */
    void failure(String documentId, String message);

    /** Emit an output document, streamed to the client in chunks (64 KiB). */
    void output(String outputDocumentId, Map<String, String> properties, InputStream data) throws IOException;

    /** Emit a small output document. */
    default void output(String outputDocumentId, Map<String, String> properties, byte[] data) throws IOException {
        output(outputDocumentId, properties, new java.io.ByteArrayInputStream(data));
    }

    /** Whether a result for this input document was already reported. */
    boolean hasResult(String documentId);
}

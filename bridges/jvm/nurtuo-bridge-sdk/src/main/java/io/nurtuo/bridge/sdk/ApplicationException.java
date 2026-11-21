// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.sdk;

/**
 * Thrown while processing one document when the target system rejected it (a business error the
 * flow may handle). Reported as DOCUMENT_STATUS_APPLICATION_ERROR; any other exception from
 * document processing is DOCUMENT_STATUS_FAILURE.
 */
public class ApplicationException extends Exception {
    private static final long serialVersionUID = 1L;

    public ApplicationException(String message) {
        super(message);
    }

    public ApplicationException(String message, Throwable cause) {
        super(message, cause);
    }
}

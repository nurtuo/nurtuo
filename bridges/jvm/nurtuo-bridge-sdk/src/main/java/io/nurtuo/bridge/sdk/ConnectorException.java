// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.sdk;

import io.grpc.Status;

/**
 * A failure that is not tied to one document and therefore fails the whole gRPC call
 * (Describe/Test/Browse/Execute). Per-document business errors use {@link ApplicationException};
 * any other exception thrown while handling one document becomes a FAILURE result for it.
 */
public class ConnectorException extends RuntimeException {
    private static final long serialVersionUID = 1L;

    /** How the failure is reported on the wire. */
    public enum Kind {
        /** Bad connection settings or options (caller error). */
        INVALID_ARGUMENT(Status.Code.INVALID_ARGUMENT),
        /** Optional operation the connector does not implement. */
        UNIMPLEMENTED(Status.Code.UNIMPLEMENTED),
        /** Target system or environment not in a state to serve the call. */
        FAILED_PRECONDITION(Status.Code.FAILED_PRECONDITION),
        /** Technical failure. */
        INTERNAL(Status.Code.INTERNAL);

        private final Status.Code code;

        Kind(Status.Code code) {
            this.code = code;
        }

        public Status.Code code() {
            return code;
        }
    }

    private final Kind kind;

    public ConnectorException(Kind kind, String message) {
        super(message);
        this.kind = kind;
    }

    public ConnectorException(Kind kind, String message, Throwable cause) {
        super(message, cause);
        this.kind = kind;
    }

    public static ConnectorException invalidArgument(String message) {
        return new ConnectorException(Kind.INVALID_ARGUMENT, message);
    }

    public static ConnectorException unimplemented(String message) {
        return new ConnectorException(Kind.UNIMPLEMENTED, message);
    }

    public Kind kind() {
        return kind;
    }
}

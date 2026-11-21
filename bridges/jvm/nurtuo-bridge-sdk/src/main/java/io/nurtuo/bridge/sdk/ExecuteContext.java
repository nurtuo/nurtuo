// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.sdk;

import nurtuo.connector.v1.Connector.OperationType;

/** Per-batch context from the ExecuteStart message. */
public final class ExecuteContext {
    private final String executionId;
    private final String stepId;
    private final OperationType operationType;
    private final String customType;
    private final String objectTypeId;
    private final Settings connection;
    private final Settings options;
    private final Redactor redactor;
    private final java.util.function.BooleanSupplier cancelled;

    public ExecuteContext(
            String executionId,
            String stepId,
            OperationType operationType,
            String customType,
            String objectTypeId,
            Settings connection,
            Settings options,
            Redactor redactor,
            java.util.function.BooleanSupplier cancelled) {
        this.executionId = executionId;
        this.stepId = stepId;
        this.operationType = operationType;
        this.customType = customType;
        this.objectTypeId = objectTypeId;
        this.connection = connection;
        this.options = options;
        this.redactor = redactor;
        this.cancelled = cancelled;
    }

    public String executionId() {
        return executionId;
    }

    public String stepId() {
        return stepId;
    }

    public OperationType operationType() {
        return operationType;
    }

    /** E.g. "SEND" for OPERATION_TYPE_EXECUTE; empty for standard operation types. */
    public String customType() {
        return customType;
    }

    public String objectTypeId() {
        return objectTypeId;
    }

    /** Resolved connection settings; secrets are present in memory only. */
    public Settings connection() {
        return connection;
    }

    /** Operation options. */
    public Settings options() {
        return options;
    }

    public Redactor redactor() {
        return redactor;
    }

    /** True once the client cancelled the call; long-running work should stop. */
    public boolean isCancelled() {
        return cancelled.getAsBoolean();
    }
}

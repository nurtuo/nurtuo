// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.sdk;

import java.util.Objects;

/**
 * A secret value injected by the runtime (memory only). {@link #toString()} never reveals it, so
 * accidental logging or string concatenation is safe; call {@link #reveal()} at the point of use.
 */
public final class Secret {
    private final String value;

    public Secret(String value) {
        this.value = Objects.requireNonNull(value, "value");
    }

    /** The clear-text value. Hand it to the client library; do not log or store it. */
    public String reveal() {
        return value;
    }

    public char[] revealChars() {
        return value.toCharArray();
    }

    @Override
    public String toString() {
        return "***";
    }
}

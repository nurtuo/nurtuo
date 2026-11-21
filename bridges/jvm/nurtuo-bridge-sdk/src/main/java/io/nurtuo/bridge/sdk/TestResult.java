// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.sdk;

/** Outcome of "test connection". The diagnostic is redacted by the SDK before it is sent. */
public record TestResult(boolean ok, String diagnostic) {
    public static TestResult ok(String diagnostic) {
        return new TestResult(true, diagnostic);
    }

    public static TestResult failed(String diagnostic) {
        return new TestResult(false, diagnostic);
    }
}

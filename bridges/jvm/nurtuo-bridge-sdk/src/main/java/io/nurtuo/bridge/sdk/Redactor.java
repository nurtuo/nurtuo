// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.sdk;

import java.util.ArrayList;
import java.util.Collection;
import java.util.Comparator;
import java.util.List;

/**
 * Scrubs known secret values out of text before it leaves the bridge (document result messages,
 * Test diagnostics, gRPC status messages). The SDK applies it automatically; connectors can use
 * it for their own log lines through {@link ExecuteContext#redactor()}.
 */
public final class Redactor {
    public static final String MASK = "***";
    private static final Redactor NONE = new Redactor(List.of());

    private final List<String> secrets;

    public Redactor(Collection<String> secretValues) {
        List<String> s = new ArrayList<>();
        for (String v : secretValues) {
            if (v != null && !v.isEmpty()) {
                s.add(v);
            }
        }
        // Longest first so that a secret containing another secret is masked as a whole.
        s.sort(Comparator.comparingInt(String::length).reversed());
        this.secrets = List.copyOf(s);
    }

    public static Redactor none() {
        return NONE;
    }

    public String redact(String text) {
        if (text == null || secrets.isEmpty()) {
            return text;
        }
        String out = text;
        for (String secret : secrets) {
            out = out.replace(secret, MASK);
        }
        return out;
    }

    public String redact(Throwable t) {
        if (t == null) {
            return null;
        }
        String msg = t.getMessage();
        return redact(t.getClass().getSimpleName() + (msg == null ? "" : ": " + msg));
    }
}

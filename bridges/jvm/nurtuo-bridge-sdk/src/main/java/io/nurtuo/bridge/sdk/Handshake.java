// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.sdk;

import java.util.Optional;

/**
 * The stdout handshake between the Nurtuo runtime (parent) and a bridge (child), as specified in
 * bridges/README.md (the single source of truth for the process protocol):
 *
 * <pre>
 * NURTUO-BRIDGE|1|&lt;network&gt;|&lt;address&gt;|&lt;contract&gt;|&lt;implementation&gt;
 * NURTUO-BRIDGE|1|tcp|127.0.0.1:43211|nurtuo.connector.v1|jvm/0.1.0
 * NURTUO-BRIDGE|1|unix|/run/nurtuo/bridge-jvm.sock|nurtuo.connector.v1|jvm/0.1.0
 * </pre>
 *
 * Exactly one such line is written to stdout, after the gRPC server is accepting connections and
 * the health service reports SERVING. Nothing else is ever written to stdout (the SDK redirects
 * System.out to stderr); logs go to stderr. The line is terminated by a single {@code \n}.
 */
public final class Handshake {
    public static final String PREFIX = "NURTUO-BRIDGE";
    public static final String VERSION = "1";
    /** The connector contract package this SDK speaks. */
    public static final String CONTRACT = "nurtuo.connector.v1";

    private Handshake() {}

    /** A parsed handshake line. */
    public static final class Info {
        private final ListenAddress address;
        private final String contract;
        private final String implementation;

        Info(ListenAddress address, String contract, String implementation) {
            this.address = address;
            this.contract = contract;
            this.implementation = implementation;
        }

        public ListenAddress address() {
            return address;
        }

        public String contract() {
            return contract;
        }

        public String implementation() {
            return implementation;
        }
    }

    /** The handshake line for the default contract and implementation "jvm". */
    public static String line(ListenAddress address) {
        return line(address, CONTRACT, "jvm");
    }

    public static String line(ListenAddress address, String contract, String implementation) {
        boolean unix = address.kind() == ListenAddress.Kind.UNIX;
        String network = unix ? "unix" : "tcp";
        String addr = unix ? address.path() : address.host() + ":" + address.port();
        return PREFIX + "|" + VERSION + "|" + network + "|" + addr + "|" + contract + "|" + implementation;
    }

    /** Parse a handshake line; empty if the line is not a (valid) handshake. */
    public static Optional<Info> parse(String line) {
        if (line == null) {
            return Optional.empty();
        }
        // The implementation field (last) may itself contain '|'.
        String[] parts = line.trim().split("\\|", 6);
        if (parts.length < 5 || !PREFIX.equals(parts[0]) || !VERSION.equals(parts[1])) {
            return Optional.empty();
        }
        try {
            ListenAddress address = ListenAddress.parse(parts[2] + ":" + parts[3]);
            return Optional.of(new Info(address, parts[4], parts.length == 6 ? parts[5] : ""));
        } catch (IllegalArgumentException e) {
            return Optional.empty();
        }
    }
}

// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.sdk;

import java.util.Objects;

/**
 * Where the bridge listens, in the same notation as the handshake line: {@code tcp:<host>:<port>}
 * (port 0 = pick a free one) or {@code unix:<path>} (Linux only).
 */
public final class ListenAddress {
    public enum Kind { TCP, UNIX }

    private final Kind kind;
    private final String host;
    private final int port;
    private final String path;

    private ListenAddress(Kind kind, String host, int port, String path) {
        this.kind = kind;
        this.host = host;
        this.port = port;
        this.path = path;
    }

    public static ListenAddress tcp(String host, int port) {
        return new ListenAddress(Kind.TCP, Objects.requireNonNull(host), port, null);
    }

    public static ListenAddress unix(String path) {
        return new ListenAddress(Kind.UNIX, null, 0, Objects.requireNonNull(path));
    }

    public static ListenAddress parse(String s) {
        if (s.startsWith("unix:")) {
            String p = s.substring("unix:".length());
            if (p.isEmpty()) {
                throw new IllegalArgumentException("empty unix socket path");
            }
            return unix(p);
        }
        if (s.startsWith("tcp:")) {
            String rest = s.substring("tcp:".length());
            int i = rest.lastIndexOf(':');
            if (i <= 0 || i == rest.length() - 1) {
                throw new IllegalArgumentException("expected tcp:<host>:<port>, got " + s);
            }
            try {
                return tcp(rest.substring(0, i), Integer.parseInt(rest.substring(i + 1)));
            } catch (NumberFormatException e) {
                throw new IllegalArgumentException("bad port in " + s);
            }
        }
        throw new IllegalArgumentException("address must start with tcp: or unix:, got " + s);
    }

    public Kind kind() {
        return kind;
    }

    public String host() {
        return host;
    }

    public int port() {
        return port;
    }

    public String path() {
        return path;
    }

    public ListenAddress withPort(int newPort) {
        return tcp(host, newPort);
    }

    @Override
    public String toString() {
        return kind == Kind.UNIX ? "unix:" + path : "tcp:" + host + ":" + port;
    }
}

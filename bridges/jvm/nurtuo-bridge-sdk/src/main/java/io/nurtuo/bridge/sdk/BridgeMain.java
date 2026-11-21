// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.sdk;

import java.io.IOException;
import java.io.PrintStream;
import java.nio.file.Path;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.ServiceLoader;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Entry point of the bridge process.
 *
 * <pre>
 * java -jar nurtuo-bridge.jar [--connector &lt;id-or-class&gt;] [--listen tcp:127.0.0.1:0 | unix:/path.sock]
 *      [--plugin-dir DIR]... [--grace-seconds N] [--exit-on-stdin-eof] [--list-connectors] [--version]
 * </pre>
 *
 * Environment (launcher protocol, bridges/README.md): {@code NURTUO_BRIDGE_LISTEN} (default for --listen),
 * {@code NURTUO_BRIDGE_CONTRACT} (acceptable contracts), {@code NURTUO_BRIDGE_TOKEN} (shared secret
 * required from clients), {@code NURTUO_BRIDGE_WATCH_STDIN=1} (same as --exit-on-stdin-eof); JVM specific:
 * {@code NURTUO_BRIDGE_PLUGIN_DIR} (path-separated plugin directories, added to --plugin-dir).
 *
 * <p>Process protocol: logs go to stderr; after the server is up exactly one line
 * {@code NURTUO-BRIDGE|1|<network>|<address>|<contract>|<implementation>} is written to stdout (see
 * {@link Handshake}); exit code 0 after a graceful shutdown (SIGTERM/SIGINT or, with
 * --exit-on-stdin-eof, parent death), 2 on bad usage, 3 when no offered contract is supported,
 * 1 on startup failure. UNVERIFIED: Java is not available in the integration environment.
 */
public final class BridgeMain {
    private static final Logger LOG = Logger.getLogger(BridgeMain.class.getName());

    private BridgeMain() {}

    public static void main(String[] args) {
        // stdout is reserved for the handshake: anything a connector or library prints goes to stderr.
        PrintStream handshakeOut = System.out;
        System.setOut(System.err);
        System.exit(run(args, handshakeOut));
    }

    static int run(String[] args, PrintStream handshakeOut) {
        String connectorName = null;
        // Launcher protocol (bridges/README.md): NURTUO_BRIDGE_LISTEN, NURTUO_BRIDGE_CONTRACT,
        // NURTUO_BRIDGE_TOKEN, NURTUO_BRIDGE_WATCH_STDIN. Command line flags override the environment.
        String envListen = System.getenv("NURTUO_BRIDGE_LISTEN");
        String listen = envListen != null && !envListen.isBlank() ? envListen.trim() : "tcp:127.0.0.1:0";
        long graceSeconds = 10;
        boolean exitOnStdinEof = "1".equals(System.getenv("NURTUO_BRIDGE_WATCH_STDIN"));
        boolean list = false;
        List<Path> pluginDirs = new ArrayList<>();
        String envDirs = System.getenv("NURTUO_BRIDGE_PLUGIN_DIR");
        if (envDirs != null && !envDirs.isBlank()) {
            for (String d : envDirs.split(java.io.File.pathSeparator)) {
                if (!d.isBlank()) {
                    pluginDirs.add(Path.of(d));
                }
            }
        }
        try {
            for (int i = 0; i < args.length; i++) {
                switch (args[i]) {
                    case "--connector":
                        connectorName = args[++i];
                        break;
                    case "--listen":
                        listen = args[++i];
                        break;
                    case "--plugin-dir":
                        pluginDirs.add(Path.of(args[++i]));
                        break;
                    case "--grace-seconds":
                        graceSeconds = Long.parseLong(args[++i]);
                        break;
                    case "--exit-on-stdin-eof":
                        exitOnStdinEof = true;
                        break;
                    case "--list-connectors":
                        list = true;
                        break;
                    case "--version":
                        String v = BridgeMain.class.getPackage().getImplementationVersion();
                        System.err.println("nurtuo-bridge-sdk " + (v == null ? "dev" : v) + " (contract nurtuo.connector.v1)");
                        return 0;
                    default:
                        return usage("unknown argument " + args[i]);
                }
            }
        } catch (ArrayIndexOutOfBoundsException | NumberFormatException e) {
            return usage("bad arguments");
        }

        // Exit code 3: none of the offered contracts is supported (bridges/README.md).
        String offered = System.getenv("NURTUO_BRIDGE_CONTRACT");
        if (offered != null && !offered.isBlank()) {
            boolean ok = false;
            for (String c : offered.split(",")) {
                if (Handshake.CONTRACT.equals(c.trim())) {
                    ok = true;
                }
            }
            if (!ok) {
                System.err.println("nurtuo-bridge: unsupported contract(s) " + offered + ", this bridge speaks " + Handshake.CONTRACT);
                return 3;
            }
        }

        ListenAddress address;
        try {
            address = ListenAddress.parse(listen);
        } catch (IllegalArgumentException e) {
            return usage(e.getMessage());
        }

        Connector connector;
        try {
            ClassLoader loader = PluginLoader.create(pluginDirs, BridgeMain.class.getClassLoader());
            Thread.currentThread().setContextClassLoader(loader);
            if (list) {
                for (Connector c : ServiceLoader.load(Connector.class, loader)) {
                    System.err.println(c.describe().getId() + " " + c.describe().getVersion() + " " + c.getClass().getName());
                }
                return 0;
            }
            connector = select(connectorName, loader);
        } catch (IOException | RuntimeException | ReflectiveOperationException e) {
            LOG.log(Level.SEVERE, "cannot load connector: " + e.getMessage(), e);
            return 1;
        }

        BridgeServer server = BridgeServer.builder(connector)
                .listen(address)
                .authToken(System.getenv("NURTUO_BRIDGE_TOKEN"))
                .gracePeriod(Duration.ofSeconds(graceSeconds))
                .build();
        Runtime.getRuntime().addShutdownHook(new Thread(server::shutdown, "nurtuo-shutdown"));
        try {
            server.start();
        } catch (IOException | RuntimeException e) {
            LOG.log(Level.SEVERE, "cannot start bridge server: " + e.getMessage(), e);
            return 1;
        }
        String v = BridgeMain.class.getPackage().getImplementationVersion();
        handshakeOut.println(Handshake.line(server.address(), Handshake.CONTRACT, "jvm/" + (v == null ? "dev" : v)));
        handshakeOut.flush();
        LOG.info("bridge ready: " + connector.describe().getId() + " on " + server.address());

        if (exitOnStdinEof) {
            Thread watcher = new Thread(() -> {
                try {
                    while (System.in.read() != -1) {
                        // discard
                    }
                } catch (IOException e) {
                    // treat as EOF
                }
                LOG.info("stdin closed, shutting down");
                server.shutdown();
            }, "nurtuo-stdin-watcher");
            watcher.setDaemon(true);
            watcher.start();
        }
        try {
            server.awaitTermination();
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            server.shutdown();
        }
        return 0;
    }

    private static Connector select(String name, ClassLoader loader) throws ReflectiveOperationException {
        if (name != null && name.contains(".")) {
            Class<?> c = Class.forName(name, true, loader);
            return c.asSubclass(Connector.class).getDeclaredConstructor().newInstance();
        }
        List<Connector> found = new ArrayList<>();
        for (Connector c : ServiceLoader.load(Connector.class, loader)) {
            found.add(c);
        }
        if (name != null) {
            for (Connector c : found) {
                if (c.describe().getId().equals(name)) {
                    return c;
                }
            }
            throw new IllegalArgumentException("no connector with id '" + name + "' (see --list-connectors)");
        }
        if (found.size() == 1) {
            return found.get(0);
        }
        throw new IllegalArgumentException(found.isEmpty()
                ? "no connector found on the classpath"
                : "several connectors found; choose one with --connector <id> (see --list-connectors)");
    }

    private static int usage(String problem) {
        System.err.println("nurtuo-bridge: " + problem);
        System.err.println("usage: --connector <id|class> --listen <tcp:host:port|unix:path> [--plugin-dir DIR]..."
                + " [--grace-seconds N] [--exit-on-stdin-eof] [--list-connectors] [--version]");
        return 2;
    }
}

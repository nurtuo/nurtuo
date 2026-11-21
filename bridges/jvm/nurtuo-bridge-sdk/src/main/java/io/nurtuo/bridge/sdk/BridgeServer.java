// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.sdk;

import io.grpc.Server;
import io.grpc.health.v1.HealthCheckResponse.ServingStatus;
import io.grpc.netty.shaded.io.grpc.netty.NettyServerBuilder;
import io.grpc.netty.shaded.io.netty.channel.EventLoopGroup;
import io.grpc.netty.shaded.io.netty.channel.epoll.Epoll;
import io.grpc.netty.shaded.io.netty.channel.epoll.EpollEventLoopGroup;
import io.grpc.netty.shaded.io.netty.channel.epoll.EpollServerDomainSocketChannel;
import io.grpc.netty.shaded.io.netty.channel.unix.DomainSocketAddress;
import io.grpc.protobuf.services.HealthStatusManager;
import java.io.IOException;
import java.net.InetSocketAddress;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.attribute.PosixFilePermissions;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.logging.Level;
import java.util.logging.Logger;
import nurtuo.connector.v1.ConnectorServiceGrpc;

/**
 * Runs a {@link Connector} as a gRPC server: the connector service, the standard
 * {@code grpc.health.v1.Health} service, graceful shutdown.
 *
 * <p>Shutdown order: health flips to NOT_SERVING, the server stops accepting new calls, in-flight
 * calls get {@code gracePeriod} to finish, then they are cancelled; the connector is closed last.
 */
public final class BridgeServer implements AutoCloseable {
    private static final Logger LOG = Logger.getLogger(BridgeServer.class.getName());
    private static final int MAX_INBOUND_MESSAGE_BYTES = 16 * 1024 * 1024;

    private final ConnectorService service;
    private final ListenAddress requested;
    private final String authToken;
    private final Duration gracePeriod;
    private final HealthStatusManager health = new HealthStatusManager();
    private final List<EventLoopGroup> eventLoops = new ArrayList<>();
    private final AtomicBoolean shutdownStarted = new AtomicBoolean();
    private Server server;
    private ListenAddress bound;

    private BridgeServer(Builder b) {
        this.service = new ConnectorService(b.connector);
        this.requested = b.listen;
        this.authToken = b.authToken;
        this.gracePeriod = b.gracePeriod;
    }

    public static Builder builder(Connector connector) {
        return new Builder(connector);
    }

    public static final class Builder {
        private final Connector connector;
        private ListenAddress listen = ListenAddress.tcp("127.0.0.1", 0);
        private String authToken;
        private Duration gracePeriod = Duration.ofSeconds(10);

        private Builder(Connector connector) {
            this.connector = connector;
        }

        public Builder listen(ListenAddress listen) {
            this.listen = listen;
            return this;
        }

        /** Require this token in the {@code x-nurtuo-bridge-token} header (health is exempt). */
        public Builder authToken(String authToken) {
            this.authToken = (authToken == null || authToken.isEmpty()) ? null : authToken;
            return this;
        }

        public Builder gracePeriod(Duration gracePeriod) {
            this.gracePeriod = gracePeriod;
            return this;
        }

        public BridgeServer build() {
            return new BridgeServer(this);
        }
    }

    /** Start listening. Returns once the server accepts connections and health is SERVING. */
    public synchronized BridgeServer start() throws IOException {
        NettyServerBuilder nb;
        if (requested.kind() == ListenAddress.Kind.UNIX) {
            nb = UnixTransport.configure(requested.path(), eventLoops);
        } else {
            nb = NettyServerBuilder.forAddress(new InetSocketAddress(requested.host(), requested.port()));
        }
        nb.maxInboundMessageSize(MAX_INBOUND_MESSAGE_BYTES)
                .addService(service)
                .addService(health.getHealthService());
        if (authToken != null) {
            nb.intercept(new AuthInterceptor(authToken));
        }
        server = nb.build().start();
        bound = requested.kind() == ListenAddress.Kind.UNIX ? requested : requested.withPort(server.getPort());
        if (requested.kind() == ListenAddress.Kind.UNIX) {
            UnixTransport.restrict(requested.path());
        }
        health.setStatus("", ServingStatus.SERVING);
        health.setStatus(ConnectorServiceGrpc.SERVICE_NAME, ServingStatus.SERVING);
        return this;
    }

    /** The address actually bound (port resolved for {@code tcp:host:0}). */
    public ListenAddress address() {
        return bound;
    }

    public void awaitTermination() throws InterruptedException {
        server.awaitTermination();
    }

    /** Graceful shutdown; idempotent and safe to call from a shutdown hook. */
    public void shutdown() {
        if (!shutdownStarted.compareAndSet(false, true) || server == null) {
            return;
        }
        health.enterTerminalState();
        server.shutdown();
        try {
            if (!server.awaitTermination(gracePeriod.toMillis(), TimeUnit.MILLISECONDS)) {
                LOG.warning("grace period elapsed, cancelling in-flight calls");
                server.shutdownNow();
                server.awaitTermination(5, TimeUnit.SECONDS);
            }
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            server.shutdownNow();
        }
        service.close();
        for (EventLoopGroup g : eventLoops) {
            g.shutdownGracefully(0, 2, TimeUnit.SECONDS);
        }
        if (requested.kind() == ListenAddress.Kind.UNIX) {
            try {
                Files.deleteIfExists(Path.of(requested.path()));
            } catch (IOException e) {
                LOG.log(Level.FINE, "could not delete socket file", e);
            }
        }
    }

    @Override
    public void close() {
        shutdown();
    }

    /** Linux-only Unix domain socket transport (epoll); isolated so other platforms never load it. */
    private static final class UnixTransport {
        static NettyServerBuilder configure(String path, List<EventLoopGroup> groups) throws IOException {
            if (!Epoll.isAvailable()) {
                throw new IOException("unix: listeners need Linux epoll (not available on this platform); "
                        + "use tcp:127.0.0.1:0 with NURTUO_BRIDGE_TOKEN instead");
            }
            Files.deleteIfExists(Path.of(path)); // stale socket from a crashed run
            EpollEventLoopGroup boss = new EpollEventLoopGroup(1);
            EpollEventLoopGroup worker = new EpollEventLoopGroup();
            groups.add(boss);
            groups.add(worker);
            return NettyServerBuilder.forAddress(new DomainSocketAddress(path))
                    .channelType(EpollServerDomainSocketChannel.class)
                    .bossEventLoopGroup(boss)
                    .workerEventLoopGroup(worker);
        }

        static void restrict(String path) {
            try {
                Files.setPosixFilePermissions(Path.of(path), PosixFilePermissions.fromString("rw-------"));
            } catch (IOException | UnsupportedOperationException e) {
                LOG.log(Level.WARNING, "could not restrict socket permissions", e);
            }
        }
    }
}

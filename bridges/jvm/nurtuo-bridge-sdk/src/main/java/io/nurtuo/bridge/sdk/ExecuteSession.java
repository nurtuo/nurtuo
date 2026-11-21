// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.sdk;

import com.google.protobuf.ByteString;
import io.grpc.Status;
import io.grpc.stub.ServerCallStreamObserver;
import io.grpc.stub.StreamObserver;
import java.io.IOException;
import java.io.InputStream;
import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.LinkedBlockingQueue;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.logging.Level;
import java.util.logging.Logger;
import nurtuo.connector.v1.Connector.DocumentChunk;
import nurtuo.connector.v1.Connector.DocumentResult;
import nurtuo.connector.v1.Connector.DocumentStatus;
import nurtuo.connector.v1.Connector.ExecuteRequest;
import nurtuo.connector.v1.Connector.ExecuteResponse;
import nurtuo.connector.v1.Connector.ExecuteStart;

/**
 * Server side of one Execute stream (see the stream protocol in connector.proto).
 *
 * <p>gRPC threads only enqueue incoming messages; the connector runs on a worker thread and pulls
 * documents through {@link Documents}. Inbound flow control is manual: one message is requested
 * from the transport each time the worker consumes one, so a slow connector back-pressures the
 * client instead of buffering the whole batch in memory. Output chunks wait for transport
 * readiness for the same reason.
 */
final class ExecuteSession implements StreamObserver<ExecuteRequest> {
    private static final Logger LOG = Logger.getLogger(ExecuteSession.class.getName());
    static final int OUTPUT_CHUNK_SIZE = 64 * 1024;
    private static final int MAX_MESSAGE_CHARS = 4096;

    private record Item(DocumentChunk chunk, Throwable error, boolean end) {}

    private final Connector connector;
    private final ServerCallStreamObserver<ExecuteResponse> out;
    private final ExecutorService executor;
    private final LinkedBlockingQueue<Item> inbox = new LinkedBlockingQueue<>();
    private final AtomicBoolean closed = new AtomicBoolean();
    private final Object readyLock = new Object();
    private volatile boolean started;
    private volatile boolean cancelled;

    private ExecuteContext ctx;
    private Redactor redactor = Redactor.none();
    private final Set<String> resulted = ConcurrentHashMap.newKeySet();

    ExecuteSession(Connector connector, ServerCallStreamObserver<ExecuteResponse> out, ExecutorService executor) {
        this.connector = connector;
        this.out = out;
        this.executor = executor;
        out.disableAutoInboundFlowControl();
        out.setOnCancelHandler(() -> {
            cancelled = true;
            inbox.add(new Item(null, new IOException("call cancelled by client"), false));
        });
        out.setOnReadyHandler(() -> {
            synchronized (readyLock) {
                readyLock.notifyAll();
            }
        });
        out.request(1); // ExecuteStart
    }

    // ---- gRPC thread ----

    @Override
    public void onNext(ExecuteRequest req) {
        try {
            switch (req.getMsgCase()) {
                case START:
                    if (started) {
                        fail(Status.INVALID_ARGUMENT.withDescription("duplicate ExecuteStart"));
                        return;
                    }
                    begin(req.getStart());
                    out.request(1);
                    break;
                case CHUNK:
                    if (!started) {
                        fail(Status.INVALID_ARGUMENT.withDescription("first message must be ExecuteStart"));
                        return;
                    }
                    inbox.add(new Item(req.getChunk(), null, false));
                    break;
                case END:
                    if (!started) {
                        fail(Status.INVALID_ARGUMENT.withDescription("first message must be ExecuteStart"));
                        return;
                    }
                    inbox.add(new Item(null, null, true));
                    break;
                case MSG_NOT_SET:
                default:
                    fail(Status.INVALID_ARGUMENT.withDescription("empty ExecuteRequest"));
            }
        } catch (ConnectorException e) {
            fail(Status.fromCode(e.kind().code()).withDescription(e.getMessage()));
        } catch (RuntimeException e) {
            LOG.log(Level.WARNING, "execute: unexpected error handling request", e);
            fail(Status.INTERNAL.withDescription(redactor.redact(e)));
        }
    }

    @Override
    public void onError(Throwable t) {
        cancelled = true;
        inbox.add(new Item(null, t, false));
    }

    @Override
    public void onCompleted() {
        // Half-close without ExecuteEnd: treat as end of input.
        inbox.add(new Item(null, null, true));
    }

    private void begin(ExecuteStart start) {
        Settings connection = new Settings(start.getConnection().getSettings(), connector.secretKeys());
        Settings options = new Settings(start.getOptions(), connector.secretKeys());
        Set<String> secrets = new LinkedHashSet<>(connection.secretValues());
        secrets.addAll(options.secretValues());
        redactor = new Redactor(secrets);
        ctx = new ExecuteContext(
                start.getExecutionId(),
                start.getStepId(),
                start.getOperationType(),
                start.getCustomType(),
                start.getObjectTypeId(),
                connection,
                options,
                redactor,
                () -> cancelled || out.isCancelled());
        started = true;
        executor.execute(this::run);
    }

    private void fail(Status status) {
        cancelled = true;
        inbox.add(new Item(null, new IOException(status.getDescription()), false));
        closeWith(status);
    }

    // ---- worker thread ----

    private void run() {
        Incoming docs = new Incoming();
        Sink sink = new Sink();
        try {
            connector.execute(ctx, docs, sink);
            // Documents the connector left unread are consumed so the protocol completes cleanly.
            while (docs.next() != null) {
                // skipped
            }
            for (String id : docs.received) {
                if (!resulted.contains(id)) {
                    sink.failure(id, "document was not processed by the connector");
                }
            }
            closeWith(Status.OK);
        } catch (Throwable t) {
            if (t instanceof InterruptedException) {
                Thread.currentThread().interrupt();
            }
            if (cancelled) {
                LOG.log(Level.FINE, "execute cancelled", t);
                closeWith(Status.CANCELLED);
                return;
            }
            LOG.log(Level.WARNING, "execute failed (step " + ctx.stepId() + ")", t);
            Status.Code code = t instanceof ConnectorException
                    ? ((ConnectorException) t).kind().code()
                    : Status.Code.INTERNAL;
            closeWith(Status.fromCode(code).withDescription(truncate(redactor.redact(t))));
        }
    }

    private void closeWith(Status status) {
        if (!closed.compareAndSet(false, true)) {
            return;
        }
        synchronized (out) {
            try {
                if (status.isOk()) {
                    out.onCompleted();
                } else {
                    out.onError(status.asRuntimeException());
                }
            } catch (RuntimeException e) {
                LOG.log(Level.FINE, "closing execute stream", e);
            }
        }
    }

    private Item take() throws InterruptedException {
        Item item = inbox.take();
        try {
            out.request(1);
        } catch (RuntimeException e) {
            // call already closed
        }
        return item;
    }

    private static String truncate(String s) {
        if (s == null) {
            return "";
        }
        return s.length() <= MAX_MESSAGE_CHARS ? s : s.substring(0, MAX_MESSAGE_CHARS) + "...";
    }

    private void awaitReady() throws IOException, InterruptedException {
        synchronized (readyLock) {
            while (!out.isReady()) {
                if (cancelled || out.isCancelled()) {
                    throw new IOException("call cancelled by client");
                }
                readyLock.wait(100);
            }
        }
    }

    // ---- input ----

    private final class Incoming implements Documents {
        final Set<String> received = new LinkedHashSet<>();
        private Doc current;
        private boolean endSeen;

        @Override
        public InputDocument next() throws IOException, InterruptedException {
            if (current != null) {
                current.skipRest();
                current = null;
            }
            if (endSeen) {
                return null;
            }
            Item item = take();
            if (item.error() != null) {
                throw new IOException("stream failed: " + item.error().getMessage(), item.error());
            }
            if (item.end()) {
                endSeen = true;
                return null;
            }
            DocumentChunk first = item.chunk();
            if (!received.add(first.getDocumentId())) {
                throw new IOException("document " + first.getDocumentId() + " sent twice");
            }
            current = new Doc(first);
            return current;
        }

        private final class Doc extends InputStream implements InputDocument {
            private final String id;
            private final Map<String, String> properties;
            private byte[] buf;
            private int pos;
            private boolean last;
            private boolean streamTaken;

            Doc(DocumentChunk first) {
                this.id = first.getDocumentId();
                this.properties = Collections.unmodifiableMap(new LinkedHashMap<>(first.getPropertiesMap()));
                this.buf = first.getData().toByteArray();
                this.last = first.getLast();
            }

            @Override
            public String id() {
                return id;
            }

            @Override
            public Map<String, String> properties() {
                return properties;
            }

            @Override
            public InputStream data() {
                if (streamTaken) {
                    throw new IllegalStateException("data() may be called once per document");
                }
                streamTaken = true;
                return this;
            }

            /** Ensure bytes are available; false at the end of this document. */
            private boolean fill() throws IOException {
                while (pos >= buf.length) {
                    if (last) {
                        return false;
                    }
                    Item item;
                    try {
                        item = take();
                    } catch (InterruptedException e) {
                        Thread.currentThread().interrupt();
                        throw new IOException("interrupted while reading document " + id, e);
                    }
                    if (item.error() != null) {
                        throw new IOException("stream failed: " + item.error().getMessage(), item.error());
                    }
                    if (item.end()) {
                        throw new IOException("stream ended in the middle of document " + id);
                    }
                    DocumentChunk c = item.chunk();
                    if (!c.getDocumentId().equals(id)) {
                        throw new IOException("protocol violation: chunk of document " + c.getDocumentId()
                                + " interleaved into document " + id);
                    }
                    buf = c.getData().toByteArray();
                    pos = 0;
                    last = c.getLast();
                }
                return true;
            }

            @Override
            public int read() throws IOException {
                if (!fill()) {
                    return -1;
                }
                return buf[pos++] & 0xff;
            }

            @Override
            public int read(byte[] b, int off, int len) throws IOException {
                if (len == 0) {
                    return 0;
                }
                if (!fill()) {
                    return -1;
                }
                int n = Math.min(len, buf.length - pos);
                System.arraycopy(buf, pos, b, off, n);
                pos += n;
                return n;
            }

            void skipRest() throws IOException {
                pos = buf.length;
                while (fill()) {
                    pos = buf.length;
                }
            }
        }
    }

    // ---- output ----

    private final class Sink implements ResultSink {
        private final Object outputLock = new Object();

        @Override
        public void success(String documentId) {
            success(documentId, Map.of(), List.of());
        }

        @Override
        public void success(String documentId, Map<String, String> tracked, List<String> outputDocumentIds) {
            send(DocumentResult.newBuilder()
                    .setDocumentId(documentId)
                    .setStatus(DocumentStatus.DOCUMENT_STATUS_SUCCESS)
                    .putAllTracked(tracked)
                    .addAllOutputDocumentIds(outputDocumentIds));
        }

        @Override
        public void applicationError(String documentId, String message) {
            error(documentId, DocumentStatus.DOCUMENT_STATUS_APPLICATION_ERROR, message);
        }

        @Override
        public void failure(String documentId, String message) {
            error(documentId, DocumentStatus.DOCUMENT_STATUS_FAILURE, message);
        }

        private void error(String documentId, DocumentStatus status, String message) {
            send(DocumentResult.newBuilder()
                    .setDocumentId(documentId)
                    .setStatus(status)
                    .setMessage(truncate(redactor.redact(message))));
        }

        private void send(DocumentResult.Builder result) {
            if (!resulted.add(result.getDocumentId())) {
                LOG.warning("duplicate result for document " + result.getDocumentId() + " ignored");
                return;
            }
            write(ExecuteResponse.newBuilder().setResult(result).build());
        }

        @Override
        public boolean hasResult(String documentId) {
            return resulted.contains(documentId);
        }

        @Override
        public void output(String outputDocumentId, Map<String, String> properties, InputStream data)
                throws IOException {
            synchronized (outputLock) { // chunks of two documents must not interleave
                try {
                    byte[] cur = data.readNBytes(OUTPUT_CHUNK_SIZE);
                    boolean first = true;
                    while (true) {
                        byte[] next = cur.length == OUTPUT_CHUNK_SIZE ? data.readNBytes(OUTPUT_CHUNK_SIZE) : new byte[0];
                        boolean isLast = next.length == 0;
                        DocumentChunk.Builder chunk = DocumentChunk.newBuilder()
                                .setDocumentId(outputDocumentId)
                                .setData(ByteString.copyFrom(cur))
                                .setLast(isLast);
                        if (first) {
                            chunk.putAllProperties(properties);
                            first = false;
                        }
                        awaitReady();
                        write(ExecuteResponse.newBuilder().setOutput(chunk).build());
                        if (isLast) {
                            return;
                        }
                        cur = next;
                    }
                } catch (InterruptedException e) {
                    Thread.currentThread().interrupt();
                    throw new IOException("interrupted while writing output " + outputDocumentId, e);
                }
            }
        }

        private void write(ExecuteResponse response) {
            synchronized (out) {
                if (closed.get()) {
                    return;
                }
                out.onNext(response);
            }
        }
    }
}

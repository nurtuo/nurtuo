# bridge-jvm (Apache-2.0)

Out-of-process connector host for the JVM: lets connectors written in Java/Kotlin (JMS, IBM MQ,
SAP JCo, JDBC, EDI, ...) speak the one Nurtuo connector contract
([`proto/nurtuo/connector/v1/connector.proto`](../../proto/nurtuo/connector/v1/connector.proto)) over
gRPC. The runtime starts the bridge as a supervised child process only when a flow uses a
connector with `host: bridge-jvm` (see [`bridges/README.md`](../README.md)).

Status: M3 scaffold. Built with Gradle (Kotlin DSL), Java 17 bytecode (`--release 17`, runs on 17+).

| Module | What |
|---|---|
| `nurtuo-bridge-sdk` | `Connector` base class, streaming Execute session, secrets/redaction, health, graceful shutdown, handshake, plugin classpath, `BridgeMain`, and `io.nurtuo.bridge.sdk.testing.ConnectorTestHarness` for tests |
| `nurtuo-connector-jms` | JMS 2.0 connector (`jakarta.jms`): `SEND`, `RECEIVE`, `REQUEST_REPLY` over a pluggable provider (Artemis tested, IBM MQ via plugin jars) |
| `nurtuo-connector-example` | Reference connector (`example-text`, `EXECUTE/TRANSFORM`), about 100 lines, with tests |
| `nurtuo-bridge-app` | Thin fat jar (SDK + grpc + connectors, never vendor jars) and the `bridgeDist` zip with launcher scripts |

## Build and test

```sh
cd bridges/jvm
gradle wrapper --gradle-version 8.14.3   # once; the wrapper is not committed yet (see Status)
./gradlew build                          # compile, generate stubs from ../../proto, run all tests
./gradlew :nurtuo-bridge-app:bridgeDist  # build/distributions/nurtuo-bridge-jvm-<version>.zip
./gradlew :nurtuo-bridge-app:shadowJar -PwithExample   # fat jar incl. the example connector
```

Requires a JDK 17+ (Gradle 8.14 itself runs on JDK 17 to 24). Stubs are generated at build time from the
repo-level `proto/` directory with `protobuf-gradle-plugin` (no copies, no vendored generated code):
messages are nested classes of `nurtuo.connector.v1.Connector` (the proto has no `java_multiple_files`)
and the service is `nurtuo.connector.v1.ConnectorServiceGrpc`.

## Process protocol (what the Go launcher relies on)

Command line (`nurtuo-bridge` script or `java -jar nurtuo-bridge.jar`):

```
--connector <id|class>    which connector to serve; optional if exactly one is on the classpath
--listen tcp:127.0.0.1:0 | unix:/path/bridge.sock     default tcp:127.0.0.1:0 (port 0 = free port)
--plugin-dir DIR          repeatable; customer jars (see Classpath plugins)
--grace-seconds N         graceful shutdown budget, default 10
--exit-on-stdin-eof       shut down when stdin closes (parent died / pipe closed)
--list-connectors | --version
```

Environment, as set by the Go launcher (`connectors/bridge`, protocol in [../README.md](../README.md),
the single spec): `NURTUO_BRIDGE_LISTEN` (default for `--listen`), `NURTUO_BRIDGE_CONTRACT`
(exit code 3 if `nurtuo.connector.v1` is not offered), `NURTUO_BRIDGE_TOKEN` (shared secret, see
below), `NURTUO_BRIDGE_WATCH_STDIN=1` (same as `--exit-on-stdin-eof`). JVM specific:
`NURTUO_BRIDGE_PLUGIN_DIR` (path-separated), `NURTUO_BRIDGE_JAVA`, `NURTUO_BRIDGE_JAVA_OPTS`
(launcher script). Command line flags override the environment.

> Status: the Java sources were adapted to the launcher protocol during integration without a JDK
> available; the Java build and tests have not been run since that change (unverified).

**Handshake.** stdout is reserved. After the gRPC server accepts connections and health reports
SERVING, exactly one line is written to stdout and flushed, in the format of
[../README.md](../README.md#handshake-bridge-to-runtime):

```
NURTUO-BRIDGE|1|<network>|<address>|<contract>|<implementation>
NURTUO-BRIDGE|1|unix|/run/nurtuo/bridge-jvm.sock|nurtuo.connector.v1|jvm/0.1.0
NURTUO-BRIDGE|1|tcp|127.0.0.1:43211|nurtuo.connector.v1|jvm/0.1.0
```

`<address>` is the socket path or `host:port` (the real port when `:0` was requested). The
launcher reads stdout until it sees this line (with a startup timeout), then dials. `System.out` is
redirected to stderr by the SDK, so connector code cannot corrupt the handshake; all logs go to
stderr (capture it for diagnostics).

**Transports.** `unix:` works on Linux only (epoll; the socket file is created `0600`, put it in a
`0700` directory). On macOS and Windows use `tcp:127.0.0.1:0`. Named pipes are not supported by
grpc-java; on Windows the loopback TCP listener plus token is the supported mode.

**Token.** A loopback port is reachable by every local user. The runtime should generate a random
token per bridge process, pass it in `NURTUO_BRIDGE_TOKEN` (not on the command line, where other
users can see it) and send it as gRPC metadata `x-nurtuo-bridge-token` on every call. Calls without
it get `UNAUTHENTICATED`. The `grpc.health.v1.Health` service is exempt (a known leniency compared
to [../README.md](../README.md), which requires the token on health too; the launcher always sends it).

**Health.** Standard `grpc.health.v1.Health/Check` for service `""` and
`nurtuo.connector.v1.ConnectorService`: `SERVING` while running, `NOT_SERVING` once shutdown starts.

**Shutdown.** SIGTERM/SIGINT (or stdin EOF with `--exit-on-stdin-eof`): health flips to
`NOT_SERVING`, no new calls are accepted, in-flight calls get `--grace-seconds`, then they are
cancelled, `Connector.close()` runs, the socket file is removed; exit code 0. Exit codes: 0 clean,
1 startup failure (message on stderr), 2 usage error. Windows has no SIGTERM: use
`--exit-on-stdin-eof` and close the child's stdin, or terminate the process after the grace period.

## Authoring a connector

Extend `io.nurtuo.bridge.sdk.Connector`, give it a public no-arg constructor, list it in
`META-INF/services/io.nurtuo.bridge.sdk.Connector`.

```java
public final class MyConnector extends Connector {
    @Override public Descriptor describe() { /* id, SemVer version, operations, schemas, capabilities */ }
    @Override public TestResult test(Settings connection) throws Exception { /* diagnostic without secrets */ }
    @Override protected void executeDocument(ExecuteContext ctx, InputDocument doc, ResultSink sink) throws Exception {
        try (InputStream in = doc.data()) {                       // streamed, never the whole batch in memory
            sink.output(doc.id() + ".out", Map.of(), transform(in)); // output documents are chunked (64 KiB)
        }
        sink.success(doc.id(), Map.of("tracked.key", "value"), List.of(doc.id() + ".out"));
    }
    // optional: browse(Settings, BrowseRequest), secretKeys(), close(), or override execute(ctx, docs, sink) for batch logic
}
```

See [`ExampleConnector`](nurtuo-connector-example/src/main/java/io/nurtuo/bridge/example/ExampleConnector.java)
and its test for a complete, runnable one.

**Execute semantics** (the stream protocol is in `connector.proto`; the SDK implements it):

- `executeDocument` is called per input document, in order. Return normally = SUCCESS (reported
  automatically unless you reported something); throw `ApplicationException` = APPLICATION_ERROR
  (target system rejected the document, flows can handle it); any other exception = FAILURE
  (technical). A per-document error is a result, never a gRPC error.
- Throw `ConnectorException` (`invalidArgument`, `unimplemented`, ...) for problems that make the
  whole call meaningless (bad options): it fails the call with that gRPC status.
- Documents the connector leaves unreported are reported FAILURE by the SDK, so a runtime never
  waits for a missing result.
- Payloads stream: `doc.data()` yields bytes as chunks arrive; unread bytes are skipped. Inbound
  flow control is manual (one message requested per message consumed) and output waits for transport
  readiness, so a slow connector back-pressures the runtime instead of buffering a batch.
- Override `execute(ctx, docs, sink)` for batch-oriented targets (bulk APIs, one transaction per batch).
  One connector instance serves all calls concurrently: keep state thread-safe, open target
  connections per call or pool them.
- `Listen` (push listeners) is not implemented by the SDK yet and answers `UNIMPLEMENTED`, which the
  contract allows.

**Secrets.** The runtime injects resolved secrets into `ConnectionConfig.settings` in memory. Any
key matching `password|passwd|passphrase|secret|token|credential|api key|private key` (any nesting depth) plus
keys returned by `Connector.secretKeys()` is a secret: read it with `settings.secret("password")`
(a `Secret` whose `toString()` is `***`), never log `reveal()`ed values, and note that the SDK
masks every known secret value in document result messages, Test diagnostics and gRPC status
messages. Do not write secrets to disk or to stdout.

**Testing.** `ConnectorTestHarness.start(connector)` serves it over an in-process channel and
gives `describe()`, a blocking stub and `execute(start, docs, chunkSize)`, which returns results
and reassembled output documents per document. For the language-neutral conformance suite, run the
bridge and point the Go `contracttest` at it:

```go
contracttest.Run(t, contracttest.Options{
    Dial: func(t testing.TB) *grpc.ClientConn { /* dial the address from the handshake line */ },
    Connection: &connectorv1.ConnectionConfig{Settings: settings},
})
```

## JMS connector

Operations (all `OPERATION_TYPE_EXECUTE`, operation in `custom_type`): `SEND`, `RECEIVE`,
`REQUEST_REPLY`. One JMS context per Execute batch.

Connection settings: `provider` (`artemis` default, `ibmmq`, `custom`), `brokerUrl`, `factoryClass`,
`factoryProperties` (bean setters on the factory), `username`, `password` (secret), `clientId`.

Options: `destination` (required), `destinationType` (`queue`|`topic`); SEND: `messageType`
(`text`|`bytes`), `deliveryMode`, `priority`, `timeToLiveMs`, `correlationId`; RECEIVE:
`maxMessages` (default 1, max 1000), `timeoutMs`, `selector`, `subscriptionName` (durable topic
subscription, needs `clientId`); REQUEST_REPLY: `timeoutMs` (default 30000), `replyQueue` (default:
temporary queue), `correlationId`.

- SEND: the document payload is the message body. Document properties become String message
  properties (characters invalid in JMS property names become `_`, e.g. `order.id` -> `order_id`);
  `jms.correlationId` and `jms.type` set those headers. Tracked: `jms.messageId`, `jms.correlationId`.
- RECEIVE: each input document is a trigger; up to `maxMessages` messages become output documents
  `<docId>-1`, `<docId>-2`, ... with properties `jms.messageId`, `jms.correlationId`, `jms.type`,
  `jms.timestamp`, `jms.redelivered`, `jms.destination`, `jms.bodyType` plus the message's own
  properties. The context is transacted: committed after the outputs of a trigger were emitted,
  rolled back if the trigger document fails (at-least-once). Map/Object/Stream message bodies are
  delivered with an empty payload and `jms.bodyType` set.
- REQUEST_REPLY: sends with `JMSReplyTo`, waits for a reply whose correlation id equals the request's
  correlation id or JMS message id (both conventions work), emits it as `<docId>-reply`. No reply
  in time is a FAILURE of that document.
- Broker unreachable or bad credentials: every document of the batch gets FAILURE (secrets masked).
  A missing provider jar fails the call with `FAILED_PRECONDITION` naming the plugin directory.

Providers are resolved by class name through the context classloader:

```json
{ "provider": "artemis", "brokerUrl": "tcp://broker:61616", "username": "app", "password": "<secret>" }
{ "provider": "ibmmq", "username": "app", "password": "<secret>",
  "factoryProperties": { "hostName": "mq.corp", "port": 1414, "queueManager": "QM1",
                         "channel": "DEV.APP.SVRCONN", "sslCipherSuite": "TLS_AES_256_GCM_SHA384" } }
```

IBM MQ (documented, not tested in CI): put the Jakarta flavour of the IBM MQ classes for JMS
(`com.ibm.mq.jakarta.client`, version 9.3+ and its dependencies) in the plugin directory; the
`ibmmq` provider uses `com.ibm.mq.jakarta.jms.MQConnectionFactory` and defaults `transportType` to 1
(client). Any other vendor: `"provider": "custom", "factoryClass": "...", "brokerUrl"` (String
constructor) and/or `factoryProperties`. Tests run against an embedded Artemis broker (in-VM, no network).

## Classpath plugins (customer-supplied jars)

SAP JCo, the IBM MQ client, Oracle/Db2 JDBC drivers and similar are never bundled: the licences do
not allow it and the bridge stays Apache-2.0. Put the jars in `<install>/plugins/` (or any
`--plugin-dir` / `NURTUO_BRIDGE_PLUGIN_DIR`); every `*.jar` there and one level below is added to a
parent-first classloader, installed as the thread context classloader of all connector threads. The
SDK, grpc and `jakarta.jms` always come from the bridge. Native libraries (JCo `sapjco3`) must be on
`-Djava.library.path` via `NURTUO_BRIDGE_JAVA_OPTS`. Restart after changing plugins. The runtime can
download/accept licences and place jars; the bridge only reads the directory.

A connector packaged as its own jar can also be dropped into the plugin directory: it is found
through `META-INF/services/io.nurtuo.bridge.sdk.Connector` and selected with `--connector <id>`.

## Packaging

`:nurtuo-bridge-app:shadowJar` merges service files and excludes signature files; the result runs with
`java -jar`. `bridgeDist` zips:

```
bin/nurtuo-bridge  bin/nurtuo-bridge.cmd   launchers (JAVA from NURTUO_BRIDGE_JAVA, bundled jre/, JAVA_HOME, PATH)
lib/nurtuo-bridge.jar
plugins/README.txt                         empty plugin directory
```

The launcher `exec`s java (signals reach the JVM) and keeps stdout clean. A variant with a bundled
`jlink` JRE under `jre/` (the launcher already prefers it) is planned but not implemented here; the
signed OCI packaging (cosign, `oras`) belongs to the release pipeline.

## Versioning

- The **bridge** has its own SemVer (`version` in the root `build.gradle.kts`, shown by `--version`
  and the jar manifest `Implementation-Version`), independent of the platform version.
- The **contract** is `nurtuo.connector.v1`; the SDK declares it in the manifest
  (`Nurtuo-Connector-Contract`). Within v1 only additive proto changes are allowed (`buf breaking`),
  so an SDK built against v1 keeps working with newer v1 runtimes; a `v2` package needs a new SDK major.
- Each **connector** has its own SemVer in its descriptor (`Descriptor.version`), e.g. `jms` 0.1.0.
  Bump minor for new options/operations, major for removed or changed ones (flow definitions pin
  connector versions).
- Dependency versions (grpc, protobuf, Artemis) are in `gradle/libs.versions.toml`.

## Status and known gaps

- No Gradle wrapper is committed yet (generate it as shown above) and the build has not been run
  by CI; it was authored on a host without a runnable JVM build toolchain.
- Not implemented: `Listen` (push listeners for JMS/IBM MQ), jlink JRE bundle, signing/OCI publish,
  Windows named pipes, macOS Unix sockets (use TCP loopback), JMS connection pooling across batches.

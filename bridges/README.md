# bridges/ (Apache-2.0)

Optional out-of-process connector hosts. Each bridge is a **separate signed artifact** (OCI, in
the connector registry), downloaded by the runtime on deploy only when a flow uses a connector
with `host: bridge-*`, started as a supervised child process and spoken to over gRPC
(`proto/nurtuo/connector/v1`) on a Unix socket / Windows named pipe.

| Dir | Stack | Build | Planned |
|---|---|---|---|
| `jvm/` | Java 21, grpc-java; SAP JCo, JMS/IBM MQ, JDBC, Smooks EDI/AS2, Saxon HE, Camel | Gradle + `jlink` JRE (with and without bundled JRE) | M3 |
| `python/` | Python 3.12+, grpcio; ML, embeddings | uv + python-build-standalone | M3 |
| `dotnet/` | .NET 8+, Grpc.AspNetCore | `dotnet publish` self-contained | M3 (optional) |

Bridges stay Apache-2.0 so customer-supplied proprietary libraries (SAP JCo, IBM MQ client,
Oracle/Db2/Teradata JDBC, Saxon PE/EE) can run inside them without touching AGPL code.
These libraries are **never bundled**; the bridge loads them from a customer-provided path or
downloads them only after the customer accepts their license.

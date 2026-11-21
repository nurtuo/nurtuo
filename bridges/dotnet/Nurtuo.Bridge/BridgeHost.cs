// SPDX-License-Identifier: Apache-2.0
using System.Net;
using System.Security.Cryptography;
using System.Text;
using Grpc.AspNetCore.HealthChecks;
using Microsoft.AspNetCore.Builder;
using Microsoft.AspNetCore.Hosting;
using Microsoft.AspNetCore.Hosting.Server;
using Microsoft.AspNetCore.Hosting.Server.Features;
using Microsoft.AspNetCore.Http;
using Microsoft.AspNetCore.Server.Kestrel.Core;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Diagnostics.HealthChecks;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Logging;

namespace Nurtuo.Bridge;

/// <summary>
/// Process protocol constants. Normative description: bridges/README.md.
/// Handshake line on stdout once the server listens:
/// <c>NURTUO-BRIDGE|1|&lt;network&gt;|&lt;address&gt;|&lt;contract&gt;|&lt;implementation&gt;</c>.
/// </summary>
public static class BridgeProtocol
{
    public const string Contract = "nurtuo.connector.v1";
    public static readonly string[] SupportedContracts = [Contract];
    public const string HandshakePrefix = "NURTUO-BRIDGE";
    public const int HandshakeVersion = 1;
    public const string TokenHeader = "x-nurtuo-bridge-token";
    public const string EnvListen = "NURTUO_BRIDGE_LISTEN";
    public const string EnvContract = "NURTUO_BRIDGE_CONTRACT";
    public const string EnvToken = "NURTUO_BRIDGE_TOKEN";
    public const string EnvWatchStdin = "NURTUO_BRIDGE_WATCH_STDIN";

    public static string FormatHandshake(string network, string address, string contract = Contract) =>
        string.Join('|', HandshakePrefix, HandshakeVersion, network, address, contract, "dotnet/" + typeof(BridgeProtocol).Assembly.GetName().Version?.ToString(3));

    /// <summary>First contract of the runtime's preference list (comma separated) that this bridge supports.</summary>
    public static string? Negotiate(string? requested)
    {
        var wanted = (string.IsNullOrWhiteSpace(requested) ? Contract : requested)
            .Split(',', StringSplitOptions.TrimEntries | StringSplitOptions.RemoveEmptyEntries);
        return wanted.FirstOrDefault(w => SupportedContracts.Contains(w));
    }
}

public sealed class BridgeOptions
{
    /// <summary><c>unix:/path/to.sock</c> or <c>tcp:127.0.0.1:0</c>.</summary>
    public string Listen { get; set; } = "tcp:127.0.0.1:0";
    /// <summary>Shared secret every RPC must carry in the <c>x-nurtuo-bridge-token</c> header (optional).</summary>
    public string? Token { get; set; }
    public int ChunkSize { get; set; } = Chunker.DefaultChunkSize;
    public int MaxMessageBytes { get; set; } = 16 * 1024 * 1024;
    public TimeSpan ShutdownGrace { get; set; } = TimeSpan.FromSeconds(5);
}

/// <summary>A started bridge server.</summary>
public sealed class RunningBridge(WebApplication app, string network, string address) : IAsyncDisposable
{
    public string Network { get; } = network;
    /// <summary>Socket path (unix) or <c>host:port</c> (tcp).</summary>
    public string Address { get; } = address;
    public string Handshake => BridgeProtocol.FormatHandshake(Network, Address);
    /// <summary>The URI clients dial (<c>http://127.0.0.1:port</c>, or <c>http://localhost</c> with a unix socket handler).</summary>
    public string Uri => Network == "tcp" ? "http://" + Address : "http://localhost";

    public Task WaitForShutdownAsync(CancellationToken ct = default) => app.WaitForShutdownAsync(ct);

    public async Task StopAsync(CancellationToken ct = default)
    {
        await app.StopAsync(ct);
        if (Network == "unix") { try { File.Delete(Address); } catch (IOException) { } }
    }

    public async ValueTask DisposeAsync()
    {
        await StopAsync();
        await app.DisposeAsync();
    }
}

public static class BridgeHost
{
    /// <summary>Starts the gRPC server (non-blocking) without any process protocol side effects.</summary>
    public static async Task<RunningBridge> StartAsync(ConnectorBase connector, BridgeOptions? options = null, Action<ILoggingBuilder>? logging = null)
    {
        options ??= new BridgeOptions();
        var (network, target) = ParseListen(options.Listen);
        if (network == "unix" && File.Exists(target)) File.Delete(target); // stale socket of a crashed predecessor

        var builder = WebApplication.CreateSlimBuilder();
        builder.Logging.ClearProviders();
        if (logging is null)
            builder.Logging.AddConsole(o => o.LogToStandardErrorThreshold = LogLevel.Trace); // stdout belongs to the handshake
        else
            logging(builder.Logging);
        builder.Services.Configure<ConsoleLifetimeOptions>(o => o.SuppressStatusMessages = true);
        builder.Services.Configure<HostOptions>(o => o.ShutdownTimeout = options.ShutdownGrace);
        builder.WebHost.ConfigureKestrel(k =>
        {
            k.Limits.MaxRequestBodySize = null;
            if (network == "unix")
                k.ListenUnixSocket(target, lo => lo.Protocols = HttpProtocols.Http2);
            else
            {
                var (host, port) = SplitHostPort(target);
                k.Listen(host, port, lo => lo.Protocols = HttpProtocols.Http2);
            }
        });
        builder.Services.AddSingleton(connector);
        builder.Services.AddSingleton(options);
        builder.Services.AddGrpc(g =>
        {
            g.MaxReceiveMessageSize = options.MaxMessageBytes;
            g.MaxSendMessageSize = options.MaxMessageBytes;
        });
        builder.Services.AddGrpcHealthChecks().AddCheck("connector", () => HealthCheckResult.Healthy());

        var app = builder.Build();
        BridgeLog.Logger = app.Services.GetRequiredService<ILoggerFactory>().CreateLogger("Nurtuo.Bridge");
        if (!string.IsNullOrEmpty(options.Token))
        {
            var expected = Encoding.UTF8.GetBytes(options.Token);
            app.Use(async (ctx, next) =>
            {
                var got = Encoding.UTF8.GetBytes(ctx.Request.Headers[BridgeProtocol.TokenHeader].ToString());
                if (got.Length != expected.Length || !CryptographicOperations.FixedTimeEquals(got, expected))
                {
                    ctx.Response.StatusCode = StatusCodes.Status401Unauthorized; // gRPC clients see UNAUTHENTICATED
                    return;
                }
                await next();
            });
        }
        app.MapGrpcService<ConnectorGrpcService>();
        app.MapGrpcHealthChecksService();

        await app.StartAsync();
        if (network == "tcp")
        {
            var addr = app.Services.GetRequiredService<IServer>().Features.Get<IServerAddressesFeature>()!.Addresses.First();
            var u = new Uri(addr);
            target = $"{u.Host}:{u.Port}";
        }
        return new RunningBridge(app, network, target);
    }

    /// <summary>
    /// Runs the connector as a bridge process: contract negotiation, serve, one handshake line on stdout,
    /// graceful shutdown on SIGTERM/SIGINT or when stdin reaches EOF (when <c>NURTUO_BRIDGE_WATCH_STDIN=1</c>).
    /// Returns the exit code (3: no acceptable contract, 2: cannot listen).
    /// </summary>
    public static async Task<int> RunAsync(ConnectorBase connector, TextWriter? handshakeOut = null)
    {
        var stdout = handshakeOut ?? Console.Out;
        if (handshakeOut is null) Console.SetOut(Console.Error); // stray Console.WriteLine must not corrupt the handshake

        var requested = Environment.GetEnvironmentVariable(BridgeProtocol.EnvContract);
        var contract = BridgeProtocol.Negotiate(requested);
        if (contract is null)
        {
            Console.Error.WriteLine($"none of the requested contracts '{requested}' is supported (supported: {string.Join(", ", BridgeProtocol.SupportedContracts)})");
            return 3;
        }
        var options = new BridgeOptions
        {
            Listen = Environment.GetEnvironmentVariable(BridgeProtocol.EnvListen) is { Length: > 0 } l ? l : "tcp:127.0.0.1:0",
            Token = Environment.GetEnvironmentVariable(BridgeProtocol.EnvToken),
        };
        RunningBridge running;
        try
        {
            running = await StartAsync(connector, options);
        }
        catch (Exception e) when (e is ArgumentException or IOException or InvalidOperationException)
        {
            Console.Error.WriteLine("cannot start: " + e.Message);
            return 2;
        }

        using var cts = new CancellationTokenSource();
        if (Environment.GetEnvironmentVariable(BridgeProtocol.EnvWatchStdin) == "1")
        {
            _ = Task.Run(async () =>
            {
                try
                {
                    await using var stdin = Console.OpenStandardInput();
                    var buf = new byte[4096];
                    while (await stdin.ReadAsync(buf) > 0) { }
                }
                catch (IOException) { }
                cts.Cancel(); // EOF: the runtime went away
            });
        }
        await stdout.WriteLineAsync(BridgeProtocol.FormatHandshake(running.Network, running.Address, contract));
        await stdout.FlushAsync();
        try
        {
            await running.WaitForShutdownAsync(cts.Token); // returns on SIGTERM/SIGINT (ConsoleLifetime) or cancellation
        }
        catch (OperationCanceledException) { }
        await running.StopAsync();
        return 0;
    }

    internal static (string Network, string Target) ParseListen(string spec)
    {
        var i = spec.IndexOf(':');
        if (i > 0)
        {
            var network = spec[..i];
            var rest = spec[(i + 1)..];
            if (network == "unix" && rest.Length > 0) return (network, rest);
            if (network == "tcp" && rest.Length > 0)
            {
                var (host, _) = SplitHostPort(rest);
                if (!host.Equals(IPAddress.Loopback) && !host.Equals(IPAddress.IPv6Loopback))
                    throw new ArgumentException($"tcp listen address must be loopback, got '{rest}'");
                return (network, rest);
            }
        }
        throw new ArgumentException($"invalid listen spec '{spec}'; expected unix:/path or tcp:127.0.0.1:port");
    }

    private static (IPAddress Host, int Port) SplitHostPort(string s)
    {
        var i = s.LastIndexOf(':');
        if (i <= 0) throw new ArgumentException($"bad host:port '{s}'");
        var host = s[..i].Trim('[', ']');
        if (host == "localhost") host = "127.0.0.1";
        if (!int.TryParse(s[(i + 1)..], out var port) || !IPAddress.TryParse(host, out var ip))
            throw new ArgumentException($"bad host:port '{s}'");
        return (ip, port);
    }
}

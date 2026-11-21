// SPDX-License-Identifier: Apache-2.0
using System.Text.RegularExpressions;
using Google.Protobuf.WellKnownTypes;

namespace Nurtuo.Bridge;

/// <summary>An opaque secret value. Call <see cref="Reveal"/> at the exact point of use.</summary>
public sealed class Secret
{
    private readonly string _value;
    public Secret(string value) => _value = value;
    public string Reveal() => _value;
    public override string ToString() => Redactor.Mask;
}

/// <summary>Masks known secret values in text. Applied by the server to everything leaving the process.</summary>
public sealed class Redactor
{
    public const string Mask = "***";
    /// <summary>Values shorter than this are not redacted (it would mangle ordinary text).</summary>
    public const int MinLength = 4;

    private readonly List<string> _values = new();

    public void Add(string value)
    {
        if (value.Length < MinLength || _values.Contains(value)) return;
        _values.Add(value);
        _values.Sort((a, b) => b.Length.CompareTo(a.Length)); // longest first: no partial leftovers
    }

    public bool IsEmpty => _values.Count == 0;

    public string Apply(string text)
    {
        foreach (var v in _values)
            text = text.Replace(v, Mask, StringComparison.Ordinal);
        return text;
    }
}

/// <summary>
/// Read-only view of <c>ConnectionConfig.settings</c>. The runtime injects resolved secrets in
/// memory only; never log, persist or echo them. Top-level keys that look like secrets (or are
/// listed in the connector's <c>SecretKeys</c>) come back as <see cref="Secret"/>.
/// </summary>
public sealed class ConnectionSettings
{
    private static readonly Regex DefaultSecretKey = new(
        "(pass(word|wd|phrase)?|secret|token|api[_-]?key|credential|private[_-]?key|authorization)",
        RegexOptions.IgnoreCase | RegexOptions.Compiled);

    private readonly Dictionary<string, object?> _raw;
    private readonly IReadOnlySet<string>? _secretKeys;

    public Redactor Redactor { get; } = new();

    public ConnectionSettings(Struct? settings, IReadOnlySet<string>? secretKeys = null)
    {
        _raw = settings is null ? new() : (Dictionary<string, object?>)Json.ToObject(settings)!;
        _secretKeys = secretKeys;
        Collect(_raw, false);
    }

    public IReadOnlyCollection<string> Keys => _raw.Keys;

    public bool Contains(string key) => _raw.ContainsKey(key);

    private bool IsSecret(string key) => _secretKeys is not null ? _secretKeys.Contains(key) : DefaultSecretKey.IsMatch(key);

    /// <summary>A plain (non-secret) setting; throws for secret keys, use <see cref="Secret"/>.</summary>
    public T? Get<T>(string key, T? fallback = default)
    {
        if (IsSecret(key)) throw new InvalidOperationException($"setting '{key}' is a secret; use Secret(\"{key}\")");
        return _raw.TryGetValue(key, out var v) && v is T t ? t : fallback;
    }

    public Secret Secret(string key) =>
        _raw.TryGetValue(key, out var v) && v is string s ? new Secret(s) : throw new KeyNotFoundException(key);

    private void Collect(object? value, bool underSecret)
    {
        switch (value)
        {
            case Dictionary<string, object?> d:
                foreach (var (k, v) in d) Collect(v, underSecret || IsSecret(k));
                break;
            case List<object?> l:
                foreach (var v in l) Collect(v, underSecret);
                break;
            case string s when underSecret:
                Redactor.Add(s);
                break;
        }
    }

    public override string ToString() => "ConnectionSettings(" + string.Join(", ", _raw.Keys.Select(k => IsSecret(k) ? $"{k}=***" : k)) + ")";
}

internal static class Json
{
    public static object? ToObject(Value v) => v.KindCase switch
    {
        Value.KindOneofCase.StructValue => ToObject(v.StructValue),
        Value.KindOneofCase.ListValue => v.ListValue.Values.Select(x => ToObject(x)).ToList(),
        Value.KindOneofCase.StringValue => v.StringValue,
        Value.KindOneofCase.NumberValue => v.NumberValue,
        Value.KindOneofCase.BoolValue => v.BoolValue,
        _ => null,
    };

    public static object ToObject(Struct s) =>
        s.Fields.ToDictionary(kv => kv.Key, kv => ToObject(kv.Value));
}

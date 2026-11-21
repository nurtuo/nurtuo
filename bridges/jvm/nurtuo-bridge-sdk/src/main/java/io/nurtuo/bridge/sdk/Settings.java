// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.sdk;

import com.google.protobuf.Struct;
import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Map;
import java.util.Optional;
import java.util.Set;
import java.util.regex.Pattern;

/**
 * Typed read access to a settings/options {@link Struct}. Values are held in memory only.
 *
 * <p>Secret detection: a key is a secret if it matches {@link #SECRET_KEY} (password, secret,
 * token, credential, api key, private key) or is listed in {@link Connector#secretKeys()}; this
 * applies at any nesting depth. Secret values feed the {@link Redactor} and are only available as
 * {@link Secret} through {@link #secret(String)}. {@link #toString()} masks them.
 */
public final class Settings {
    public static final Pattern SECRET_KEY =
            Pattern.compile("(?i).*(pass(word|wd|phrase)|secret|token|credential|api[-_]?key|private[-_]?key).*");

    private final Map<String, Object> values;
    private final Set<String> extraSecretKeys;

    public Settings(Struct struct, Set<String> extraSecretKeys) {
        this(Structs.toMap(struct), extraSecretKeys);
    }

    private Settings(Map<String, Object> values, Set<String> extraSecretKeys) {
        this.values = values;
        this.extraSecretKeys = extraSecretKeys;
    }

    public static Settings empty() {
        return new Settings(new LinkedHashMap<>(), Set.of());
    }

    public static Settings of(Map<String, Object> values) {
        return new Settings(new LinkedHashMap<>(values), Set.of());
    }

    public boolean has(String key) {
        return values.get(key) != null;
    }

    public Optional<String> string(String key) {
        Object v = values.get(key);
        if (v == null) {
            return Optional.empty();
        }
        if (v instanceof Double && ((Double) v) % 1 == 0) {
            return Optional.of(Long.toString(((Double) v).longValue()));
        }
        return Optional.of(v.toString());
    }

    public String string(String key, String defaultValue) {
        return string(key).orElse(defaultValue);
    }

    /** @throws ConnectorException INVALID_ARGUMENT when the key is missing or blank */
    public String requireString(String key) {
        String s = string(key).orElse("");
        if (s.isBlank()) {
            throw ConnectorException.invalidArgument("missing required setting '" + key + "'");
        }
        return s;
    }

    public long integer(String key, long defaultValue) {
        Object v = values.get(key);
        if (v == null) {
            return defaultValue;
        }
        if (v instanceof Number) {
            return ((Number) v).longValue();
        }
        try {
            return Long.parseLong(v.toString().trim());
        } catch (NumberFormatException e) {
            throw ConnectorException.invalidArgument("setting '" + key + "' must be an integer");
        }
    }

    public boolean bool(String key, boolean defaultValue) {
        Object v = values.get(key);
        if (v == null) {
            return defaultValue;
        }
        return v instanceof Boolean ? (Boolean) v : Boolean.parseBoolean(v.toString());
    }

    /** Nested object (empty settings if absent). */
    @SuppressWarnings("unchecked")
    public Settings object(String key) {
        Object v = values.get(key);
        if (v instanceof Map) {
            return new Settings((Map<String, Object>) v, extraSecretKeys);
        }
        return new Settings(new LinkedHashMap<>(), extraSecretKeys);
    }

    @SuppressWarnings("unchecked")
    public List<Object> list(String key) {
        Object v = values.get(key);
        return v instanceof List ? (List<Object>) v : List.of();
    }

    public Set<String> keys() {
        return Collections.unmodifiableSet(values.keySet());
    }

    /** Raw value (String, Double, Boolean, Map, List or null). Prefer the typed accessors. */
    public Object raw(String key) {
        return values.get(key);
    }

    /** A secret setting; empty if absent. */
    public Optional<Secret> secret(String key) {
        Object v = values.get(key);
        return v == null ? Optional.empty() : Optional.of(new Secret(v.toString()));
    }

    public boolean isSecretKey(String key) {
        return SECRET_KEY.matcher(key).matches() || extraSecretKeys.contains(key);
    }

    /** All secret values at any depth, for building a {@link Redactor}. */
    public Set<String> secretValues() {
        Set<String> out = new LinkedHashSet<>();
        collect(values, false, out);
        return out;
    }

    @SuppressWarnings("unchecked")
    private void collect(Map<String, Object> map, boolean parentSecret, Set<String> out) {
        for (Map.Entry<String, Object> e : map.entrySet()) {
            boolean secret = parentSecret || isSecretKey(e.getKey());
            collectValue(e.getValue(), secret, out);
        }
    }

    @SuppressWarnings("unchecked")
    private void collectValue(Object v, boolean secret, Set<String> out) {
        if (v instanceof Map) {
            collect((Map<String, Object>) v, secret, out);
        } else if (v instanceof List) {
            for (Object item : (List<Object>) v) {
                collectValue(item, secret, out);
            }
        } else if (secret && v != null) {
            out.add(v instanceof Double && ((Double) v) % 1 == 0
                    ? Long.toString(((Double) v).longValue())
                    : v.toString());
        }
    }

    @Override
    public String toString() {
        Map<String, Object> masked = new LinkedHashMap<>();
        for (Map.Entry<String, Object> e : values.entrySet()) {
            masked.put(e.getKey(), isSecretKey(e.getKey()) ? Redactor.MASK : e.getValue());
        }
        return masked.toString();
    }
}

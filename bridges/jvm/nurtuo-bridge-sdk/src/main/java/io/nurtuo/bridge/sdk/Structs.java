// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.sdk;

import com.google.protobuf.ListValue;
import com.google.protobuf.NullValue;
import com.google.protobuf.Struct;
import com.google.protobuf.Value;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/** Conversions between protobuf {@link Struct} and plain Java maps (String/Double/Boolean/List/Map/null). */
public final class Structs {
    private Structs() {}

    public static Map<String, Object> toMap(Struct struct) {
        Map<String, Object> out = new LinkedHashMap<>();
        if (struct == null) {
            return out;
        }
        for (Map.Entry<String, Value> e : struct.getFieldsMap().entrySet()) {
            out.put(e.getKey(), fromValue(e.getValue()));
        }
        return out;
    }

    public static Object fromValue(Value v) {
        switch (v.getKindCase()) {
            case STRING_VALUE:
                return v.getStringValue();
            case NUMBER_VALUE:
                return v.getNumberValue();
            case BOOL_VALUE:
                return v.getBoolValue();
            case STRUCT_VALUE:
                return toMap(v.getStructValue());
            case LIST_VALUE:
                List<Object> list = new ArrayList<>();
                for (Value item : v.getListValue().getValuesList()) {
                    list.add(fromValue(item));
                }
                return list;
            case NULL_VALUE:
            case KIND_NOT_SET:
            default:
                return null;
        }
    }

    public static Struct fromMap(Map<String, ?> map) {
        Struct.Builder b = Struct.newBuilder();
        for (Map.Entry<String, ?> e : map.entrySet()) {
            b.putFields(e.getKey(), toValue(e.getValue()));
        }
        return b.build();
    }

    /** Build a Struct from alternating key/value arguments: {@code of("type", "object", "required", List.of("a"))}. */
    public static Struct of(Object... keyValues) {
        if (keyValues.length % 2 != 0) {
            throw new IllegalArgumentException("key/value arguments must come in pairs");
        }
        Map<String, Object> m = new LinkedHashMap<>();
        for (int i = 0; i < keyValues.length; i += 2) {
            m.put(String.valueOf(keyValues[i]), keyValues[i + 1]);
        }
        return fromMap(m);
    }

    @SuppressWarnings("unchecked")
    public static Value toValue(Object o) {
        if (o == null) {
            return Value.newBuilder().setNullValue(NullValue.NULL_VALUE).build();
        }
        if (o instanceof Value) {
            return (Value) o;
        }
        if (o instanceof Struct) {
            return Value.newBuilder().setStructValue((Struct) o).build();
        }
        if (o instanceof String) {
            return Value.newBuilder().setStringValue((String) o).build();
        }
        if (o instanceof Boolean) {
            return Value.newBuilder().setBoolValue((Boolean) o).build();
        }
        if (o instanceof Number) {
            return Value.newBuilder().setNumberValue(((Number) o).doubleValue()).build();
        }
        if (o instanceof Map) {
            return Value.newBuilder().setStructValue(fromMap((Map<String, ?>) o)).build();
        }
        if (o instanceof Iterable) {
            ListValue.Builder lb = ListValue.newBuilder();
            for (Object item : (Iterable<Object>) o) {
                lb.addValues(toValue(item));
            }
            return Value.newBuilder().setListValue(lb).build();
        }
        return Value.newBuilder().setStringValue(o.toString()).build();
    }
}

// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.jms;

import io.nurtuo.bridge.sdk.ConnectorException;
import io.nurtuo.bridge.sdk.Settings;
import jakarta.jms.ConnectionFactory;
import jakarta.jms.JMSContext;
import java.lang.reflect.InvocationTargetException;
import java.lang.reflect.Method;
import java.util.Locale;
import java.util.Map;

/**
 * Pluggable JMS provider: builds a {@link ConnectionFactory} from connection settings.
 *
 * <p>Providers are identified by the vendor's factory class name, loaded through the context
 * classloader (so jars from the bridge's plugin directory work) and configured through bean
 * setters, which is how every JMS vendor documents its factory:
 *
 * <ul>
 *   <li>{@code provider: "artemis"} (default): {@value #ARTEMIS_FACTORY} built with the
 *       {@code brokerUrl} constructor argument, e.g. {@code tcp://broker:61616}.
 *   <li>{@code provider: "ibmmq"}: {@value #IBMMQ_FACTORY} (Jakarta flavour of the IBM MQ classes
 *       for JMS, jar supplied by the customer) configured with {@code factoryProperties} such as
 *       {@code hostName, port, queueManager, channel}; {@code transportType} defaults to 1 (client).
 *   <li>{@code provider: "custom"} or any {@code factoryClass}: any class implementing
 *       {@code jakarta.jms.ConnectionFactory}; {@code brokerUrl}, if present, is passed to a
 *       String constructor, otherwise the no-argument constructor is used.
 * </ul>
 */
final class JmsConnectionFactories {
    static final String ARTEMIS_FACTORY = "org.apache.activemq.artemis.jms.client.ActiveMQJMSConnectionFactory";
    static final String IBMMQ_FACTORY = "com.ibm.mq.jakarta.jms.MQConnectionFactory";

    private JmsConnectionFactories() {}

    static ConnectionFactory create(Settings connection) {
        String provider = connection.string("provider", "artemis").toLowerCase(Locale.ROOT);
        String className = connection.string("factoryClass").orElse(null);
        Settings props = connection.object("factoryProperties");
        java.util.Map<String, Object> effective = new java.util.LinkedHashMap<>();
        for (String k : props.keys()) {
            effective.put(k, props.raw(k));
        }
        switch (provider) {
            case "artemis":
                if (className == null) {
                    className = ARTEMIS_FACTORY;
                }
                break;
            case "ibmmq":
                if (className == null) {
                    className = IBMMQ_FACTORY;
                }
                effective.putIfAbsent("transportType", 1); // WMQConstants.WMQ_CM_CLIENT
                break;
            case "custom":
                if (className == null) {
                    throw ConnectorException.invalidArgument("provider 'custom' needs 'factoryClass'");
                }
                break;
            default:
                throw ConnectorException.invalidArgument(
                        "unknown provider '" + provider + "' (use artemis, ibmmq or custom)");
        }
        String brokerUrl = connection.string("brokerUrl").orElse(null);
        if (provider.equals("artemis") && brokerUrl == null) {
            throw ConnectorException.invalidArgument("missing required setting 'brokerUrl' (e.g. tcp://localhost:61616)");
        }
        Object factory = instantiate(className, brokerUrl);
        for (Map.Entry<String, Object> e : effective.entrySet()) {
            apply(factory, e.getKey(), e.getValue());
        }
        if (!(factory instanceof ConnectionFactory)) {
            throw ConnectorException.invalidArgument(className + " is not a jakarta.jms.ConnectionFactory");
        }
        return (ConnectionFactory) factory;
    }

    /** Opens a context with the configured credentials and client id. The caller closes it. */
    static JMSContext openContext(Settings connection, int sessionMode) {
        ConnectionFactory factory = create(connection);
        String user = connection.string("username").orElse(null);
        JMSContext ctx;
        if (user != null && !user.isEmpty()) {
            String password = connection.secret("password").map(s -> s.reveal()).orElse(null);
            ctx = factory.createContext(user, password, sessionMode);
        } else {
            ctx = factory.createContext(sessionMode);
        }
        try {
            connection.string("clientId").ifPresent(ctx::setClientID);
        } catch (RuntimeException e) {
            ctx.close();
            throw e;
        }
        return ctx;
    }

    private static Object instantiate(String className, String brokerUrl) {
        Class<?> cls = loadClass(className);
        try {
            if (brokerUrl != null) {
                return cls.getConstructor(String.class).newInstance(brokerUrl);
            }
            return cls.getConstructor().newInstance();
        } catch (NoSuchMethodException e) {
            throw ConnectorException.invalidArgument(className
                    + (brokerUrl != null ? " has no (String brokerUrl) constructor" : " has no no-argument constructor"));
        } catch (InvocationTargetException e) {
            throw new ConnectorException(
                    ConnectorException.Kind.INVALID_ARGUMENT,
                    "cannot create " + className + ": " + e.getCause(),
                    e.getCause());
        } catch (ReflectiveOperationException e) {
            throw new ConnectorException(
                    ConnectorException.Kind.INVALID_ARGUMENT, "cannot create " + className + ": " + e, e);
        }
    }

    private static Class<?> loadClass(String className) {
        ClassLoader tccl = Thread.currentThread().getContextClassLoader();
        try {
            return Class.forName(className, true, tccl != null ? tccl : JmsConnectionFactories.class.getClassLoader());
        } catch (ClassNotFoundException | LinkageError e) {
            try {
                return Class.forName(className, true, JmsConnectionFactories.class.getClassLoader());
            } catch (ClassNotFoundException | LinkageError e2) {
                throw new ConnectorException(
                        ConnectorException.Kind.FAILED_PRECONDITION,
                        "JMS provider class " + className
                                + " not found: put the provider jar(s) in the bridge plugin directory",
                        e2);
            }
        }
    }

    private static void apply(Object bean, String property, Object value) {
        if (property.isEmpty()) {
            return;
        }
        String setter = "set" + Character.toUpperCase(property.charAt(0)) + property.substring(1);
        for (Method m : bean.getClass().getMethods()) {
            if (!m.getName().equals(setter) || m.getParameterCount() != 1) {
                continue;
            }
            Object arg = convert(value, m.getParameterTypes()[0]);
            if (arg == null) {
                continue;
            }
            try {
                m.invoke(bean, arg);
                return;
            } catch (InvocationTargetException e) {
                throw new ConnectorException(
                        ConnectorException.Kind.INVALID_ARGUMENT,
                        "factory property '" + property + "' rejected: " + e.getCause(),
                        e.getCause());
            } catch (IllegalAccessException e) {
                throw new ConnectorException(ConnectorException.Kind.INVALID_ARGUMENT, "factory property '" + property + "'", e);
            }
        }
        throw ConnectorException.invalidArgument("factory property '" + property + "' is not supported by "
                + bean.getClass().getName() + " (no matching setter " + setter + ")");
    }

    /** Converts a JSON-ish value to the setter parameter type; null if not convertible. */
    private static Object convert(Object v, Class<?> type) {
        if (v == null) {
            return null;
        }
        try {
            if (type == String.class) {
                return v instanceof Double && ((Double) v) % 1 == 0 ? Long.toString(((Double) v).longValue()) : v.toString();
            }
            if (type == int.class || type == Integer.class) {
                return v instanceof Number ? Integer.valueOf(((Number) v).intValue()) : Integer.valueOf(v.toString().trim());
            }
            if (type == long.class || type == Long.class) {
                return v instanceof Number ? Long.valueOf(((Number) v).longValue()) : Long.valueOf(v.toString().trim());
            }
            if (type == boolean.class || type == Boolean.class) {
                return v instanceof Boolean ? v : Boolean.valueOf(v.toString());
            }
            if (type == double.class || type == Double.class) {
                return v instanceof Number ? Double.valueOf(((Number) v).doubleValue()) : Double.valueOf(v.toString().trim());
            }
        } catch (NumberFormatException e) {
            return null;
        }
        return null;
    }
}

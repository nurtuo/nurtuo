// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.sdk;

import java.io.IOException;
import java.net.URL;
import java.net.URLClassLoader;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import java.util.logging.Logger;
import java.util.stream.Stream;

/**
 * Builds the classloader for customer-supplied jars (SAP JCo, IBM MQ client, JDBC drivers, ...)
 * that are never bundled with the bridge. Every {@code *.jar} directly in, or one level below, a
 * plugin directory is added. The loader is parent-first, so the SDK, grpc and jakarta APIs always
 * come from the bridge and a plugin cannot replace them. Native libraries (for example JCo's
 * {@code sapjco3.dll/.so}) are not loaded by this class: put their directory on
 * {@code -Djava.library.path} via {@code NURTUO_BRIDGE_JAVA_OPTS}.
 */
public final class PluginLoader {
    private static final Logger LOG = Logger.getLogger(PluginLoader.class.getName());

    private PluginLoader() {}

    public static List<Path> findJars(List<Path> dirs) throws IOException {
        List<Path> jars = new ArrayList<>();
        for (Path dir : dirs) {
            if (!Files.isDirectory(dir)) {
                throw new IOException("plugin directory does not exist: " + dir);
            }
            try (Stream<Path> s = Files.walk(dir, 2)) {
                s.filter(p -> Files.isRegularFile(p) && p.getFileName().toString().endsWith(".jar"))
                        .sorted()
                        .forEach(jars::add);
            }
        }
        return jars;
    }

    /** A loader over the plugin jars, or {@code parent} itself when there are none. */
    public static ClassLoader create(List<Path> dirs, ClassLoader parent) throws IOException {
        List<Path> jars = findJars(dirs);
        if (jars.isEmpty()) {
            return parent;
        }
        URL[] urls = new URL[jars.size()];
        for (int i = 0; i < urls.length; i++) {
            urls[i] = jars.get(i).toUri().toURL();
            LOG.info("plugin jar: " + jars.get(i).getFileName());
        }
        return new URLClassLoader("nurtuo-plugins", urls, parent);
    }
}

// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.sdk;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertSame;
import static org.junit.jupiter.api.Assertions.assertThrows;

import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.List;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

class PluginLoaderTest {
    @Test
    void findsJarsInTheDirectoryAndOneLevelBelow(@TempDir Path dir) throws IOException {
        Files.createFile(dir.resolve("b.jar"));
        Files.createFile(dir.resolve("a.jar"));
        Files.createFile(dir.resolve("notes.txt"));
        Path sub = Files.createDirectory(dir.resolve("mq"));
        Files.createFile(sub.resolve("mq-client.jar"));
        Path deep = Files.createDirectories(sub.resolve("x").resolve("y"));
        Files.createFile(deep.resolve("too-deep.jar"));

        List<String> names = PluginLoader.findJars(List.of(dir)).stream()
                .map(p -> p.getFileName().toString())
                .toList();
        assertEquals(List.of("a.jar", "b.jar", "mq-client.jar"), names.stream().sorted().toList());
    }

    @Test
    void emptyDirectoryKeepsTheParentLoader(@TempDir Path dir) throws IOException {
        ClassLoader parent = getClass().getClassLoader();
        assertSame(parent, PluginLoader.create(List.of(dir), parent));
    }

    @Test
    void missingDirectoryIsAnError(@TempDir Path dir) {
        assertThrows(IOException.class, () -> PluginLoader.findJars(List.of(dir.resolve("nope"))));
    }
}

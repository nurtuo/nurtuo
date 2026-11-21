// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.sdk;

import java.io.IOException;
import java.io.InputStream;
import java.util.Map;

/**
 * One input document of an Execute batch. The payload is streamed: {@link #data()} yields bytes as
 * chunks arrive and blocks while waiting for the next chunk, so large payloads never have to fit in
 * memory. The stream is valid until the next call to {@link Documents#next()}; unread bytes are
 * skipped automatically.
 */
public interface InputDocument {
    String id();

    /** Document properties (sent with the first chunk). */
    Map<String, String> properties();

    /** The payload stream; may be read at most once. */
    InputStream data();

    /** Convenience: read the whole payload into memory. Avoid for payloads that can be large. */
    default byte[] readAll() throws IOException {
        try (InputStream in = data()) {
            return in.readAllBytes();
        }
    }
}

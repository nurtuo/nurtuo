// SPDX-License-Identifier: Apache-2.0
package io.nurtuo.bridge.sdk;

import java.io.IOException;

/** The input side of an Execute batch: documents in arrival order. */
public interface Documents {
    /**
     * The next document, or {@code null} when the client has sent ExecuteEnd. Blocks until the
     * first chunk of the next document (or the end) has arrived. Any unread payload of the
     * previous document is discarded.
     *
     * @throws IOException if the client cancelled or the stream broke
     */
    InputDocument next() throws IOException, InterruptedException;
}

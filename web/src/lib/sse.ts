// SSE-over-fetch. The browser's native EventSource cannot set custom
// headers, and this backend's only auth mechanism IS two custom headers
// (X-Nexus-Tenant-ID / X-Nexus-User-ID, read fresh per-request) -- so this
// hand-rolled parser is required, not a style choice.
//
// Frames look like:
//   event: <type>\n
//   data: <json>\n
//   \n
// (a blank line terminates a frame; multiple `data:` lines would be
// newline-joined per the SSE spec, though this backend only ever emits one).

export interface SSEFrame {
  event: string;
  data: string;
}

function parseFrame(raw: string): SSEFrame | null {
  let event = "message";
  const dataLines: string[] = [];
  for (const line of raw.split("\n")) {
    if (line.startsWith("event:")) {
      event = line.slice("event:".length).trim();
    } else if (line.startsWith("data:")) {
      dataLines.push(line.slice("data:".length).trim());
    }
    // (id:/retry: fields aren't emitted by this backend; ignored if present.)
  }
  if (dataLines.length === 0) return null;
  return { event, data: dataLines.join("\n") };
}

/**
 * Reads a text/event-stream response body, invoking onFrame for each
 * complete `event:`/`data:` frame as it arrives. Resolves when the stream
 * ends (server closes the connection) or the abort signal fires.
 */
export async function readSSEStream(
  response: Response,
  onFrame: (frame: SSEFrame) => void,
  signal?: AbortSignal,
): Promise<void> {
  if (!response.body) {
    throw new Error("response has no body; streaming is unsupported in this environment");
  }
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";

  const cancel = () => {
    reader.cancel().catch(() => {});
  };
  signal?.addEventListener("abort", cancel);

  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });

      let sepIndex: number;
      while ((sepIndex = buffer.indexOf("\n\n")) !== -1) {
        const rawFrame = buffer.slice(0, sepIndex);
        buffer = buffer.slice(sepIndex + 2);
        const frame = parseFrame(rawFrame);
        if (frame) onFrame(frame);
      }
    }
  } finally {
    signal?.removeEventListener("abort", cancel);
  }
}

export type ConnectionState = "connecting" | "open" | "closed" | "error";

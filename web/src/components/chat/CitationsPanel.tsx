// Renders platform/retrieve's tool_result (internal/tools/builtin/retrieve.go:
// {"chunks":[{doc_id, chunk_id, chunk_index, content, distance, source_digest}]})
// as a "Sources" list -- this backend has no dedicated citations event; this
// is a pure frontend re-labeling of an existing tool's real output.
interface RetrievedChunk {
  doc_id?: string;
  chunk_id?: string;
  chunk_index?: number;
  content?: string;
  distance?: number;
}

export function CitationsPanel({ output }: { output: unknown }) {
  const chunks = extractChunks(output);
  if (!chunks || chunks.length === 0) return null;

  return (
    <div className="citations">
      <div className="citations-label">Sources</div>
      {chunks.map((c, i) => (
        <div className="citation-item" key={c.chunk_id ?? i}>
          <span className="citation-marker">[{i + 1}]</span>
          <div className="citation-body">
            <div className="citation-doc mono">
              {c.doc_id ?? "unknown document"}
              {c.chunk_index !== undefined ? ` · chunk ${c.chunk_index}` : ""}
            </div>
            {c.content && <div className="citation-excerpt">{truncate(c.content, 240)}</div>}
          </div>
        </div>
      ))}
    </div>
  );
}

function extractChunks(output: unknown): RetrievedChunk[] | null {
  if (typeof output !== "object" || output === null) return null;
  const chunks = (output as Record<string, unknown>).chunks;
  if (!Array.isArray(chunks)) return null;
  return chunks as RetrievedChunk[];
}

function truncate(s: string, max: number): string {
  return s.length > max ? s.slice(0, max) + "…" : s;
}

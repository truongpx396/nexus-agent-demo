import { renderMarkdown } from "../../lib/markdown";
import type { TimelineItem } from "../../lib/timeline";

type Props = { item: Extract<TimelineItem, { kind: "user" | "assistant" | "system" }> };

export function MessageBubble({ item }: Props) {
  if (item.kind === "system") {
    return (
      <div className={`msg system tone-${item.tone}`}>
        <span>{item.text}</span>
      </div>
    );
  }

  if (item.kind === "user") {
    return (
      <div className="msg user">
        <span className="msg-text">{item.text}</span>
      </div>
    );
  }

  return (
    <div className="msg assistant">
      <span className="msg-text" dangerouslySetInnerHTML={{ __html: renderMarkdown(item.text) }} />
    </div>
  );
}

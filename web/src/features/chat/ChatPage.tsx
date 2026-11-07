import { useParams } from "react-router-dom";
import { ChatThread } from "./ChatThread";
import { Sidebar } from "./Sidebar";

export function ChatPage() {
  const { id } = useParams<{ id: string }>();

  return (
    <div className="chat-page">
      <Sidebar />
      <ChatThread runId={id} />
    </div>
  );
}

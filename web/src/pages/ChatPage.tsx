import { useParams } from "react-router-dom";
import { ChatThread } from "../components/chat/ChatThread";
import { Sidebar } from "../components/Sidebar";

export function ChatPage() {
  const { id } = useParams<{ id: string }>();

  return (
    <div className="chat-page">
      <Sidebar />
      <ChatThread runId={id} />
    </div>
  );
}

import { Route, Routes } from "react-router-dom";
import { Header } from "./shared/ui/Header";
import { ChatPage } from "./features/chat/ChatPage";
import { Approvals } from "./features/approvals/Approvals";
import { ApprovalDetail } from "./features/approvals/ApprovalDetail";

export default function App() {
  return (
    <div className="app-shell">
      <Header />
      <main>
        <Routes>
          <Route path="/" element={<ChatPage />} />
          <Route path="/runs/:id" element={<ChatPage />} />
          <Route path="/approvals" element={<Approvals />} />
          <Route path="/approvals/:id" element={<ApprovalDetail />} />
          <Route path="*" element={<p className="page">Not found.</p>} />
        </Routes>
      </main>
    </div>
  );
}

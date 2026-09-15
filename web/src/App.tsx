import { Route, Routes } from "react-router-dom";
import { Header } from "./components/Header";
import { ChatPage } from "./pages/ChatPage";
import { Approvals } from "./pages/Approvals";
import { ApprovalDetail } from "./pages/ApprovalDetail";

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

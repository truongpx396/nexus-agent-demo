import { Route, Routes } from "react-router-dom";
import { Header } from "./components/Header";
import { NewRun } from "./pages/NewRun";
import { RunDetail } from "./pages/RunDetail";
import { Approvals } from "./pages/Approvals";
import { ApprovalDetail } from "./pages/ApprovalDetail";

export default function App() {
  return (
    <div className="app-shell">
      <Header />
      <main>
        <Routes>
          <Route path="/" element={<NewRun />} />
          <Route path="/runs/:id" element={<RunDetail />} />
          <Route path="/approvals" element={<Approvals />} />
          <Route path="/approvals/:id" element={<ApprovalDetail />} />
          <Route path="*" element={<p className="page">Not found.</p>} />
        </Routes>
      </main>
    </div>
  );
}

import { useQuery, useQueryClient, useMutation } from "@tanstack/react-query";
import { Navigate, Route, Routes, Link } from "react-router-dom";
import { Telescope, LogOut } from "lucide-react";
import { api, describeError } from "@/api/client";
import { ErrorBox, Spinner } from "@/components/bits";
import { LoginPage } from "@/pages/LoginPage";
import { ServicesPage } from "@/pages/ServicesPage";
import { ServiceDetailPage } from "@/pages/ServiceDetailPage";

export function App() {
  // The session probe is unauthenticated on the Go side precisely so
  // this can run before any cookie exists.
  const session = useQuery({ queryKey: ["session"], queryFn: api.session, retry: false });
  const qc = useQueryClient();
  const logout = useMutation({
    mutationFn: api.logout,
    onSuccess: () => void qc.invalidateQueries(),
  });

  if (session.isLoading) return <Spinner label="Starting…" />;
  if (session.error)
    return (
      <div className="mx-auto mt-24 max-w-md px-4">
        <ErrorBox message={describeError(session.error)} onRetry={() => void session.refetch()} />
      </div>
    );

  if (!session.data?.authenticated) return <LoginPage session={session.data!} />;

  return (
    <div className="min-h-full">
      <header className="border-b border-line">
        <div className="mx-auto flex max-w-6xl items-center gap-3 px-4 py-3">
          <Link to="/" className="flex items-center gap-2 font-semibold">
            <Telescope className="h-4 w-4 text-accent" />
            Observatory
          </Link>
          <span className="text-xs text-faint">deploys · health · telemetry</span>
          <button
            className="btn ml-auto inline-flex items-center gap-1.5"
            onClick={() => logout.mutate()}
            disabled={logout.isPending}
          >
            <LogOut className="h-3.5 w-3.5" /> Sign out
          </button>
        </div>
      </header>
      <main className="mx-auto max-w-6xl px-4 py-6">
        <Routes>
          <Route path="/" element={<ServicesPage />} />
          <Route path="/services/:stack" element={<ServiceDetailPage />} />
          {/* Unknown paths go home rather than rendering blank. */}
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </main>
    </div>
  );
}

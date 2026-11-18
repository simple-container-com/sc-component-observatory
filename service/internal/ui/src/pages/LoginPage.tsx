import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api, describeError } from "@/api/client";
import type { SessionInfo } from "@/api/types";

export function LoginPage({ session }: { session: SessionInfo }) {
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const qc = useQueryClient();

  const login = useMutation({
    mutationFn: () => api.login(password),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ["session"] }),
    onError: (e) => setError(describeError(e)),
  });

  // A deployment with no password configured is a DIFFERENT problem
  // from a wrong password, and the session probe reports which. Saying
  // so here saves an operator from retyping a password that can never
  // be accepted.
  if (!session.password_set) {
    return (
      <div className="mx-auto mt-24 max-w-md px-4">
        <div className="card p-6">
          <h1 className="text-lg font-semibold">Observatory</h1>
          <p className="mt-3 text-sm text-danger">No operator password is configured on this deployment.</p>
          <p className="mt-2 text-sm text-muted">
            Set <code className="font-mono text-xs">operatorPassword</code> on the{" "}
            <code className="font-mono text-xs">sc-observatory</code> resource and redeploy.
          </p>
        </div>
      </div>
    );
  }

  return (
    <div className="mx-auto mt-24 max-w-md px-4">
      <form
        className="card p-6"
        onSubmit={(e) => {
          e.preventDefault();
          setError(null);
          login.mutate();
        }}
      >
        <h1 className="text-lg font-semibold">Observatory</h1>
        <p className="mt-1 text-sm text-muted">Fleet deploy dashboard</p>
        <input
          className="input mt-5"
          type="password"
          autoFocus
          placeholder="Operator password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
        />
        {error && <p className="mt-2 text-xs text-danger">{error}</p>}
        <button className="btn mt-4 w-full" type="submit" disabled={!password || login.isPending}>
          {login.isPending ? "Signing in…" : "Sign in"}
        </button>
      </form>
    </div>
  );
}

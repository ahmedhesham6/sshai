import { createFileRoute } from "@tanstack/react-router";
import { callbackHandler } from "../../../server/auth";

export const Route = createFileRoute("/api/auth/callback")({
  server: { handlers: { GET: callbackHandler } },
});

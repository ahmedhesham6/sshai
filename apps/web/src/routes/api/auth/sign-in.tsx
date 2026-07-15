import { createFileRoute } from "@tanstack/react-router";
import { signInHandler } from "../../../server/auth";

export const Route = createFileRoute("/api/auth/sign-in")({
  server: { handlers: { GET: signInHandler } },
});

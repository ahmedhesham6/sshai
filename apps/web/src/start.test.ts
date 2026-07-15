import { describe, expect, it } from "vitest";
import { csrfSymbol } from "@tanstack/react-start";
import { startInstance } from "./start";

describe("request security", () => {
  it("rejects cross-site server functions before authentication", async () => {
    const { requestMiddleware } = await startInstance.getOptions();
    if (!requestMiddleware) {
      throw new Error("request middleware is not configured");
    }
    const [security, authentication] = requestMiddleware;
    if (!security.options.server) {
      throw new Error("security middleware has no server handler");
    }

    expect(csrfSymbol in security).toBe(true);
    expect(csrfSymbol in authentication).toBe(false);

    const response = await security.options.server({
      context: undefined,
      handlerType: "serverFn",
      next: async () => {
        throw new Error("cross-site request reached authentication");
      },
      pathname: "/_server/example",
      request: new Request("https://app.example/_server/example", {
        headers: { "Sec-Fetch-Site": "cross-site" },
        method: "POST",
      }),
    });

    expect(response).toBeInstanceOf(Response);
    expect((response as Response).status).toBe(403);
  });
});

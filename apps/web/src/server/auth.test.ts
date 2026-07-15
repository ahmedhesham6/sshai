import { describe, expect, it } from "vitest";
import { createSignInHandler } from "./auth";

describe("WorkOS route adapters", () => {
  it("starts sign-in once and preserves the local return path", async () => {
    const calls: unknown[] = [];
    const handler = createSignInHandler(async (options) => {
      calls.push(options);
      return "https://auth.example/authorize";
    });

    const response = await handler({
      request: new Request(
        "https://app.example/api/auth/sign-in?returnPathname=%2Fenvironments",
      ),
    });

    expect(response.status).toBe(307);
    expect(response.headers.get("Location")).toBe(
      "https://auth.example/authorize",
    );
    expect(calls).toEqual([{ data: { returnPathname: "/environments" } }]);
  });
});

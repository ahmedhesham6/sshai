import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { Home } from "./home";

afterEach(cleanup);

describe("Home", () => {
  it("sends a signed-out developer to WorkOS", () => {
    render(<Home signInUrl="https://auth.example/sign-in" user={null} />);

    const signIn = screen.getByRole("link", { name: "Sign in" });
    expect(signIn.getAttribute("href")).toBe("https://auth.example/sign-in");
    expect(
      screen.getByText("Remote environments that stop when the work does."),
    ).toBeTruthy();
  });

  it("teaches a signed-in developer how to create the first Environment", () => {
    render(
      <Home
        signInUrl="https://auth.example/sign-in"
        user={{ firstName: "Ada" }}
      />,
    );

    expect(
      screen.getByRole("heading", { name: "No environments yet" }),
    ).toBeTruthy();
    expect(
      screen.getByText("Run devm inside a repository to create one."),
    ).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Sign in" })).toBeNull();
  });
});

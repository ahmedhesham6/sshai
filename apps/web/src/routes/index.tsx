import { createFileRoute } from "@tanstack/react-router";
import { getAuth } from "@workos/authkit-tanstack-react-start";
import { Home } from "../components/home";

export const Route = createFileRoute("/")({
  loader: async () => {
    const { user } = await getAuth();
    return { user };
  },
  component: Index,
});

function Index() {
  const { user } = Route.useLoaderData();
  return <Home signInUrl="/api/auth/sign-in" user={user} />;
}

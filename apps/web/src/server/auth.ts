import {
  getSignInUrl,
  handleCallbackRoute,
} from "@workos/authkit-tanstack-react-start";

type ResolveSignInUrl = (
  options?: Parameters<typeof getSignInUrl>[0],
) => Promise<string>;

export function createSignInHandler(
  resolveSignInUrl: ResolveSignInUrl = getSignInUrl,
) {
  return async ({ request }: { request: Request }) => {
    const returnPathname = new URL(request.url).searchParams.get(
      "returnPathname",
    );
    const location = await resolveSignInUrl(
      returnPathname ? { data: { returnPathname } } : undefined,
    );

    return new Response(null, {
      headers: { Location: location },
      status: 307,
    });
  };
}

export const signInHandler = createSignInHandler();
export const callbackHandler = handleCallbackRoute();

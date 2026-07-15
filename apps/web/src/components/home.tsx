type User = {
  firstName?: string | null;
};

type HomeProps = {
  signInUrl: string;
  user: User | null;
};

export function Home({ signInUrl, user }: HomeProps) {
  if (user) {
    return (
      <main className="empty-state">
        <p className="eyebrow">Environment control plane</p>
        <h1>No environments yet</h1>
        <p>Run devm inside a repository to create one.</p>
        <code>devm</code>
      </main>
    );
  }

  return (
    <main className="welcome">
      <p className="eyebrow">sshai</p>
      <h1>Remote environments that stop when the work does.</h1>
      <p>
        Durable project state, private SSH, and agent-ready tooling from one
        command.
      </p>
      <a className="primary-action" href={signInUrl}>
        Sign in
      </a>
    </main>
  );
}

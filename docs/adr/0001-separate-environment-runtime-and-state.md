# Separate Environment identity, Runtime compute, and State Components

The product promises durable work with disposable or stopped compute, so `Environment`, `Runtime`, and `StateComponent` are separate concepts. This prevents an EC2 instance or disk identifier from becoming product identity and allows runtime replacement without redefining the user's workspace.

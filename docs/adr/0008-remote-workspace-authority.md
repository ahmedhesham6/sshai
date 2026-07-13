# Make the remote workspace authoritative after creation

A Project Seed initializes an Environment from the developer's local Git state, after which the remote `workspace` State Component is authoritative. Automatic local-to-remote or bidirectional synchronization is rejected because it would make dirty Git state and conflict behavior unsafe and difficult to explain.

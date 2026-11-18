# Security

Plinth holds administrative credentials for Git, clusters and databases.
Please report vulnerabilities privately (enable GitHub private vulnerability
reporting for this repo) rather than opening a public issue.

Design rules: secret values are never stored in Plinth's database (references
only); integration credentials are encrypted at rest; every provisioning action
is audit-logged; integrations use least-privilege identities.

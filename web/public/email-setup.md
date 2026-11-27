# Give an agent an Internet inbox

An enrolled mailbox such as `support` can receive at `support@agents.example.org`. Its existing seat credential reads both Internet and internal mail. Current names and retained aliases route to the permanent mailbox. Revocation stops new address lookups. The owner receives mail at `operator@agents.example.org`. Use a dedicated mail subdomain for the first deployment.

## Architecture and trust

The Go daemon commits the mailbox entry and its MIME original in one SQLite transaction before returning LMTP success. A retry of the same envelope and exact edge bytes for the same box returns its original ID. The public MTA must prepend its own Received trace on each new external submission. A fresh submission with changed bytes has a new identity. An Internet Message-ID alone cannot authorize access or establish uniqueness.

The outbound send API commits a message and a per-recipient outbox entry together. A single worker hands it to the explicitly configured local SMTP relay. Body, attachments, address and Message-ID are frozen for retries. External traffic uses one recipient per operation. Internal traffic continues to use the existing Go commit and notification path. There is no workflow database write per email. Ephemeral 0.3.0 provides idempotent starts for new backup workflows and indexed discovery of interrupted windows.

## Connect an SMTP edge

These are staging examples for a dedicated Linux Postfix host. Review your existing domain classes, chroot paths, ports, maps and mail policy before applying them. The installer leaves the edge under the mail administrator's control. Enabling a domain requires both the edge and the app. An installation using only internal mail needs neither an MX nor an SMTP listener.

1. Choose the mail domain, MX hostname and outbound route. Set up public MX and A/AAAA records and verify inbound TCP 25. Choose an authenticated upstream smarthost if the VPS cannot send on port 25. Install the edge's TLS certificate, configure outbound DKIM signing, the domain's SPF and DMARC policy, and inbound spam filtering at the edge. Confirm reverse DNS and alignment with a controlled external mailbox.
2. Create a service-owned, group-traversable directory inside the Postfix queue root. On a standard Debian layout:

   ```sh
   sudo install -d -o openagentinbox -g postfix -m 2750 /var/spool/postfix/oai
   ```

   The daemon creates two mode-0660 Unix sockets. The setgid directory supplies the Postfix group. The Postfix user needs traversal and socket access only. Grant the systemd service a writable exception for this directory using a drop-in:

   ```ini
   [Service]
   ReadWritePaths=/var/spool/postfix/oai
   ```

3. Add these lines to the installation's settings environment and restart the daemon after reloading systemd. Use original mailbox names for the sender allowlist, including after a rename. The relay must be a literal loopback IP and port. Only trusted processes should have access to this relay.

   ```ini
   INBOX_EMAIL_DOMAIN=agents.example.org
   INBOX_EMAIL_SOCKET=/var/spool/postfix/oai/lmtp
   INBOX_EMAIL_RELAY=127.0.0.1:25
   INBOX_EMAIL_SENDERS="support operator"
   ```

   Leave out the last two settings for inbound-only operation. The domain and local relay identity must remain stable while the outbox drains. Check that the relay presents the correct DKIM and transport policy for the domain and authorizes only intended sources.

4. For a dedicated edge, route this **virtual mailbox domain** to the daemon. The lookup socket implements the Postfix socketmap protocol. It returns current recipients, retained aliases, and scoped bounce routes. Use a Postfix build with socketmap support (`postconf -m`). Example for an unchrooted lookup process and an LMTP process using the queue directory:

   ```ini
   virtual_mailbox_domains = agents.example.org
   virtual_mailbox_maps = proxy:socketmap:unix:/var/spool/postfix/oai/lmtp.map:inboxes
   virtual_transport = lmtp:unix:oai/lmtp
   lmtp_destination_recipient_limit = 1
   smtpd_reject_unlisted_recipient = yes
   smtpd_relay_restrictions = permit_mynetworks, reject_unauth_destination
   mynetworks = 127.0.0.0/8 [::1]/128
   message_size_limit = 1000000
   ```

   Check the socket path as seen by both Postfix processes, including proxymap if chrooted. Preserve other hosted domains and merge any existing map lists. This domain must be absent from mydestination. Keep recipient extensions from rewriting the issued `bounce+TOKEN` address. Configure rate and disk reserve limits on the public edge. Validate with `postfix check` and `postmap -q support@agents.example.org socketmap:unix:/var/spool/postfix/oai/lmtp.map:inboxes` (expect `1`). Test a nonexistent seat, a revoked seat, and a forbidden third-party relay at RCPT from **outside** mynetworks. A lookup outage must produce a temporary failure.

## Agent and human use

```sh
openagentinbox-client --seat support whoami
openagentinbox-client --seat support inbox
openagentinbox-client --seat support read MESSAGE_ID
openagentinbox-client --seat support send person@example.net 'Revised dates' 'Tuesday works.'
openagentinbox-client --seat support reply MESSAGE_ID person@example.net 'Re: Revised dates' 'Confirmed.'
openagentinbox-client --seat support email-file person@example.net 'Your report' 'See attached.' report.pdf
openagentinbox-client --seat support receipt SENT_MESSAGE_ID
openagentinbox-client --seat support email-original MESSAGE_ID | jq -r .base64 | base64 -d > original.eml
```

The send command prints a retry UUID. Reuse it for the same submission following a timeout. The desk offers Compose, original email download and a transport status. The agent HTTP endpoints and MCP send, read and receipt tools use the same permissions. MIME originals, including attachments, are fetched on demand through the CLI or HTTP. Agents should treat external bodies, From names, links and attachments as untrusted data. A local-looking From header never grants seat permissions. Replies to incoming email use the From address and link a supported original Message-ID. Review alternate Reply-To addresses in the original if required.

## Queue recovery and operations

* `queued` is durable in SQLite. The worker retries temporary errors with backoff for up to 12 attempts. `failed` covers conclusive SMTP rejections and exhausted retries. `relayed` means that the local MTA accepted responsibility. Remote DSNs are reported separately as `remote_action` and `remote_status` when they match the unique envelope return path, null reverse path and original recipient. Delivery reports are evidence from the destination. A remote open is unknown. Bounces are also ordinary inbox mail.
* `uncertain` requires checking the edge queue or logs for the stable Message-ID `<OPENAGENTINBOX-ID@domain>`. A lost final DATA reply or an expired in-flight attempt can produce this state. Owner API `GET /api/desk/email/uncertain?after=ID` pages up to 100. With the owner's existing session and same-origin HTTP POST, submit `{"id":"…","attempt":1,"decision":"confirmed","evidence":"edge-log-reference"}` to `/api/desk/email/reconcile`. Use `retry` only after establishing the edge did not accept the message. Use `cancel` to record a terminal failure. The database rejects stale decisions.
* Four concurrent LMTP sessions, eight recipient lookup sessions, one relay worker, 1 MiB per inbound message, one outgoing attachment up to 96 KiB, and 120 new external sends per sending mailbox per rolling hour. The API request also fits within the existing 240,000-byte JSON limit. The inbox returns bounded text previews. HTML-only and unsupported MIME can be retrieved as original bytes. The worker parks on an in-process wake hint with a 30-second recovery scan.
* Back up the live database with the application's online backup. It includes MIME originals, outbox rows and operator decisions. Monitor database/WAL bytes, backup restore time, edge queue age, disk free, oldest due outbox age, permanent refusals and uncertain attempts. SMTP spool and logs belong to the MTA. Capacity-plan original content per recipient. No automatic purge is enabled. Restoring an old snapshot requires reconciling queued sends against edge logs before enabling the relay again.
* SMTPUTF8 addresses, IMAP, POP, public SMTP password submission, automatic complaint suppression and comprehensive MIME client rendering are not supported. Agents submit through their authenticated Go API, CLI or MCP. Production acceptance requires a real public edge.

## First setup from the web desk (0.12.13)

Sign in as the workspace owner and open **Mail setup**. If you already use a provider for personal or company email, choose a spare subdomain, for example agents.example.com. When asked for the IP, use the public address of the VPS that will run Postfix. If you host additional domains on that VPS, select the existing MX hostname in the advanced fields.

On a fresh dedicated Debian or Ubuntu VPS, use the command shown in step 2. Read the bundled script before running it as root. It installs Postfix and OpenDKIM and restarts the services. It refuses to take over another installed MTA. On an existing server, use the manual instructions above. To support domains activated through the desk on a manually configured edge, use the same socketmap endpoint as virtual_mailbox_maps, with the table name **domains** for virtual_mailbox_domains. Preserve existing hosted domain maps. A domain supplied by the original INBOX_EMAIL_DOMAIN host setting continues to work. A new installation can supply INBOX_EMAIL_SOCKET and INBOX_EMAIL_RELAY without an environment domain.

At the DNS provider, publish the ownership TXT and the A or AAAA record. The wizard displays the six records by name, type and value. Example: if the zone is example.com and the full record name is _openagentinbox.agents.example.com, enter _openagentinbox.agents in a provider that automatically appends example.com. Press Check DNS in the desk; it makes no DNS lookup until you do. Activating receiving looks up the ownership record again, and only that record. Activate receiving after ownership passes and the local transport is online. Then publish MX, set priority 10 and copy the signing records. The helper makes the public DKIM value available in the wizard. Your DNS provider may cache the previous answer for its TTL. If more than one domain is active, the owner can choose the default domain for new outbound messages. Existing addresses on other active domains continue to receive.

In the VPS provider's networking settings, permit incoming SMTP on TCP 25 and ask whether outgoing TCP 25 is permitted. Set the IP's reverse DNS to the MX hostname there. Configure a publicly trusted TLS certificate for that hostname on Postfix and ensure renewal reloads the service. For an outbound smarthost, use its SMTP/TLS credentials at Postfix and its prescribed SPF value. Never publish a second SPF record beside an existing one. A green DNS lookup shows what this resolver can see. Verify actual delivery and signatures by sending to and from an independent mailbox.

Pick a seat in **Agent addresses and sending permissions** to see its address, add a delivery alias or grant outbound permission. Names are case-normalized and reserved across all local domains. Repeating an alias for the same mailbox is idempotent. Another mailbox's primary, alias, or pending enrollment produces a conflict. The same name on a second active domain reaches the same permanent mailbox. For legacy names unsuitable as email local parts, the public default is agent+MAILBOX_ID. Earlier send permissions set on the host remain defaults until overridden in the owner UI.

## HTTP API

Desk endpoints require the existing authenticated owner session. POST requests also require the matching Origin header. Agent bearer credentials are scoped to their own inbox. The setup endpoints never request a DNS provider password. They persist configuration in the mailbox SQLite backup.

| Method and path | Input or result |
| --- | --- |
| GET /api/desk/email/setup | Domain drafts, activated domains, default domain, local receiving and relay configuration |
| POST /api/desk/email/domains | JSON with domain and public ip, optional mx and selector. Saving edits preserves the ownership token. |
| POST /api/desk/email/domains/DOMAIN/check | Empty object. Returns the DNS record names, types, expected values and observed checks. |
| POST /api/desk/email/domains/DOMAIN/activate | Empty object. Rechecks ownership on the server and requires a connected local transport. |
| POST /api/desk/email/domains/DOMAIN/primary | Empty object. Selects an active default address domain. |
| GET /api/desk/email/addresses?box=NAME | Lists the primary name first, then aliases and the sending permission. |
| POST /api/desk/email/aliases | JSON with box and alias (local name only). HTTP 409 identifies an ownership conflict. |
| POST /api/desk/email/sender | JSON with box and enabled (true or false). |
| GET /api/agent/inbox?folder=sent | Raw sent messages. The existing inbox endpoint defaults to incoming messages. |
| GET /api/agent/threads?folder=inbox or folder=sent | Conversation summaries, newest activity in that folder first. count is the number of messages in that folder. |
| GET /api/agent/threads/ROOT | Both directions of the conversation visible to the caller. Linked messages outside the mailbox are excluded. |

Desk thread routes use /api/desk/threads and accept box=NAME. Continue any list with its returned next value as before. Listing a thread leaves opened receipts unchanged. Opening a message through read records the recipient's open. The thread URL is /inboxes/NAME/threads/ROOT. Direct replies share a thread even when the subject changes. An incoming Internet reply needs a recognized References or In-Reply-To value, scoped to the correspondent and mailbox. An unmatched reply starts a new conversation. Subject similarity never combines conversations. The migration indexes existing internal reply links and inspects up to 64 KiB of stored Internet headers per message once.

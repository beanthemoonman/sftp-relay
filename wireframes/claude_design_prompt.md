# Claude Design Prompt — sftp-relay wireframes

Paste the block below into Claude Design.

---

Design wireframes for **sftp-relay**, a self-hosted web app for browsing remote SFTP
servers and downloading files onto a home NAS. It runs on a Raspberry Pi on a private
LAN. Single user, behind HTTP basic auth.

**Design mobile-first.** The most common real use is standing in the kitchen, opening
this on a phone, finding a file on a remote server, and queueing it to a NAS folder.
Desktop is the wider variant of the same flow, not a separate design.

Produce two breakpoints for every screen: **375px mobile** and **1280px desktop**.

## Visual direction

Dark-first, utilitarian, dense. Think a well-made file manager, not a marketing site.
Neutral greys with a single accent colour used only for primary actions and active
progress. Monospace for paths, file sizes and transfer speeds; a normal sans for
everything else. Tight vertical rhythm — this app shows lists of hundreds of files and
whitespace is expensive. No illustrations, no gradients, no hero sections.

Touch targets minimum 44px on mobile even though the design is dense. Long file paths
must truncate in the middle, not the end — the filename is the part that matters.

## Screens

### 1. Browse (the primary screen)

The core flow is: pick a server → navigate its files → pick a NAS destination → queue.

- **Desktop:** two panes side by side. Left is the remote server file listing, right is
  the NAS destination folder tree. A persistent action bar spans the bottom.
- **Mobile:** one pane at a time as sequential steps, with a step indicator and a way
  to go back. Never two panes side by side on a phone.

Remote pane needs: a server selector at the top, a breadcrumb path that collapses
sensibly when deep, and a file list showing name, size, modified date, and a
directory/file indicator. Directories sort first. Rows are multi-selectable via
checkboxes — both individual files and whole directories can be selected.

When anything is selected, a **selection bar** appears showing the count and combined
size, with a primary "Download to..." action.

Destination pane is directories only, with a breadcrumb, a "new folder" action, and
available free space shown somewhere unobtrusive.

Design these states: empty directory, loading, connection failed (with a retry action),
and permission denied.

### 2. Queue

Live view of active and pending transfers. Each job is a card showing: source server,
filename or directory name (middle-truncated), destination path, a progress bar,
percentage, transfer speed, ETA, and a cancel control. Queued jobs show position in
line rather than a progress bar.

Include a compact header summarising aggregate state — how many active, how many
queued, combined throughput.

Design these states: empty queue, one job running, five jobs with two running and
three queued, and a job that failed with an error message plus a retry action.

On mobile, cards stack full-width. On desktop, consider a denser row layout but keep
the progress bar prominent.

### 3. History

Completed, failed and cancelled transfers. Filterable by status and by server,
searchable by name. Each row: status indicator, name, server, destination, size,
duration, and completion time. A failed row can expand to reveal the error output.

Include a bulk "clear completed" affordance.

### 4. Servers

List of configured SFTP servers as cards showing name, host:port, username, and a
last-connection-status indicator. Each has edit and delete.

Add/edit form fields: display name, host, port, username, auth method toggle
(password or private key), the corresponding credential field, optional key passphrase,
and a default starting remote path. A **Test Connection** button shows an inline
result — success with round-trip timing, or a specific failure reason.

Design the delete confirmation.

### 5. Settings

Grouped sections:
- **NAS connection** — host, SSH username, key path, with its own test action
- **Allowed destination roots** — an editable list; the app refuses to write outside these
- **Transfer tuning** — concurrent jobs, parallel segments per job, both as steppers
  with plain-language explanations of the tradeoff
- **History retention** — how long completed jobs are kept

### Global chrome

- Mobile: bottom tab bar with four items — Browse, Queue, History, Settings. Servers
  lives under Settings on mobile.
- Desktop: a slim left sidebar with all five destinations.
- The Queue item shows a badge with the active job count.
- A global connection-status indicator showing whether the live event stream is
  connected — this app leans on a persistent SSE connection and the user needs to
  know when it drops.

## Deliverables

Wireframes for all five screens at both breakpoints, plus the listed states for Browse
and Queue. Include a small component inventory: file row, job card, breadcrumb,
selection bar, status pill, empty state, error state.

Grey-box wireframes with real-looking content are fine — do not invent branding.
Use plausible filenames, realistic file sizes, and real-looking NAS paths such as
`/volume1/media/downloads`.

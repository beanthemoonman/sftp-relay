import { isActive } from "../api";
import { JobCard, useJobs } from "../JobCard";
import { Empty, Err } from "../ui";

export default function Queue() {
  const jobs = useJobs();
  // Oldest first: that is the order the scheduler will actually start them in.
  const active = (jobs.data ?? []).filter(isActive).sort((a, b) => a.id - b.id);
  let waiting = 0;

  return (
    <div className="space-y-3">
      <Err error={jobs.error} />
      {jobs.isLoading && <Empty>Loading…</Empty>}
      {!jobs.isLoading && active.length === 0 && <Empty>Nothing in the queue.</Empty>}
      {active.map((j) => (
        <JobCard key={j.id} job={j} position={j.status === "queued" ? ++waiting : undefined} />
      ))}
    </div>
  );
}

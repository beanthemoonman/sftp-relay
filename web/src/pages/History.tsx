import { isActive } from "../api";
import { JobCard, useJobs } from "../JobCard";
import { Empty, Err } from "../ui";

export default function History() {
  const jobs = useJobs();
  const past = (jobs.data ?? []).filter((j) => !isActive(j)).sort((a, b) => b.id - a.id);

  return (
    <div className="space-y-3">
      <Err error={jobs.error} />
      {jobs.isLoading && <Empty>Loading…</Empty>}
      {!jobs.isLoading && past.length === 0 && <Empty>No finished jobs yet.</Empty>}
      {past.map((j) => (
        <JobCard key={j.id} job={j} />
      ))}
    </div>
  );
}

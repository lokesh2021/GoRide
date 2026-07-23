// Inline busy spinner used inside async buttons and pending states. Pure CSS
// (see styles.css .spinner) — no assets, no deps.

export function Spinner({ label }: { label?: string }) {
  return (
    <span className="spin-wrap">
      <span className="spinner" aria-hidden="true" />
      {label != null && <span>{label}</span>}
    </span>
  );
}

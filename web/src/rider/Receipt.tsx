// Itemised trip receipt, shared by the rider's CompletedView and the History
// detail. Renders a GoRide-branded receipt with route, time span, per-line
// fare items and a total, plus a "Download receipt" button that triggers a
// print-styled (@media print) window.print() — no PDF library.
//
// All money is integer paise. The trip-metric fields (distance_m/duration_s/
// started_at/ended_at) are optional: older receipts predate them, so each
// dependent line is rendered only when its data is present.

import { placeName } from "../config/personas";
import type { LatLng } from "../lib/geo";
import { rupees, surgeLabel } from "../lib/money";

export interface ReceiptModel {
  rideId: string;
  /** Header date/time (ISO) — booking or completion time. */
  dateISO: string;
  pickup: LatLng | null;
  drop: LatLng | null;
  base: number;
  distanceComponent: number;
  timeComponent: number;
  surgeX100: number;
  total: number;
  distanceM?: number;
  durationS?: number;
  startedAt?: string;
  endedAt?: string;
  method?: string;
  paid?: boolean;
}

function shortId(id: string): string {
  return id.slice(0, 8).toUpperCase();
}

function clock(iso?: string): string | null {
  if (!iso) return null;
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return null;
  return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

export function Receipt({ model }: { model: ReceiptModel }) {
  const {
    rideId, dateISO, pickup, drop, base, distanceComponent, timeComponent,
    surgeX100, total, distanceM, durationS, startedAt, endedAt, method, paid,
  } = model;

  const span = [clock(startedAt), clock(endedAt)].filter(Boolean).join(" – ");

  return (
    <div className="receipt printable">
      <div className="rcpt-head">
        <div className="rcpt-brand">
          <span className="go">Go</span>
          <span className="ride">Ride</span>
        </div>
        <div className="rcpt-title">Trip receipt</div>
        <div className="rcpt-sub">
          #{shortId(rideId)} · {new Date(dateISO).toLocaleDateString([], { day: "numeric", month: "short", year: "numeric" })}
          {clock(dateISO) ? ` · ${clock(dateISO)}` : ""}
        </div>
      </div>

      <div className="rcpt-route">
        <div className="rcpt-leg">
          <span className="marker pickup" />
          <span>{pickup ? placeName(pickup[0], pickup[1]) : "Pickup"}</span>
        </div>
        <div className="rcpt-leg">
          <span className="marker drop" />
          <span>{drop ? placeName(drop[0], drop[1]) : "Destination"}</span>
        </div>
        {span && <div className="rcpt-span">Trip time {span}</div>}
      </div>

      <div className="rcpt-items">
        <div className="rcpt-line">
          <span>Base fare</span>
          <span>{rupees(base)}</span>
        </div>
        <div className="rcpt-line">
          <span>Distance{distanceM != null ? ` (${(distanceM / 1000).toFixed(1)} km)` : ""}</span>
          <span>{rupees(distanceComponent)}</span>
        </div>
        <div className="rcpt-line">
          <span>Time{durationS != null ? ` (${Math.max(1, Math.round(durationS / 60))} min)` : ""}</span>
          <span>{rupees(timeComponent)}</span>
        </div>
        {surgeX100 > 100 && (
          <div className="rcpt-line">
            <span>Surge {surgeLabel(surgeX100 / 100)} applied</span>
            <span>included</span>
          </div>
        )}
        <div className="rcpt-hairline" />
        <div className="rcpt-line total">
          <span>Total</span>
          <span>{rupees(total)}</span>
        </div>
      </div>

      <div className="rcpt-pay">
        <span className="rcpt-method">{(method ?? "").toUpperCase() || "Payment"}</span>
        <span className={`badge ${paid ? "green" : "gray"}`}>{paid ? "Paid" : "Due"}</span>
      </div>

      <button className="btn dark no-print" onClick={() => window.print()} style={{ marginTop: 12 }}>
        Download receipt
      </button>
    </div>
  );
}

// buildFromBreakdown normalises a history receipt (breakdown jsonb map + item
// coords) into a ReceiptModel.
export function receiptFromBreakdown(
  rideId: string,
  dateISO: string,
  pickup: LatLng | null,
  drop: LatLng | null,
  breakdown: Record<string, unknown>,
  total: number,
  paid: boolean,
): ReceiptModel {
  const num = (k: string): number => (typeof breakdown[k] === "number" ? (breakdown[k] as number) : 0);
  const optNum = (k: string): number | undefined =>
    typeof breakdown[k] === "number" ? (breakdown[k] as number) : undefined;
  const str = (k: string): string | undefined =>
    typeof breakdown[k] === "string" ? (breakdown[k] as string) : undefined;
  return {
    rideId,
    dateISO,
    pickup,
    drop,
    base: num("base"),
    distanceComponent: num("distance_component"),
    timeComponent: num("time_component"),
    surgeX100: num("surge_x100") || 100,
    total,
    distanceM: optNum("distance_m"),
    durationS: optNum("duration_s"),
    startedAt: str("started_at"),
    endedAt: str("ended_at"),
    method: str("method"),
    paid,
  };
}

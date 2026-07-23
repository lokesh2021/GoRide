// Geometry helpers for map animation and route simulation.

export type LatLng = [number, number];

const EARTH_R = 6371000;

export function haversine(a: LatLng, b: LatLng): number {
  const rlat1 = (a[0] * Math.PI) / 180;
  const rlat2 = (b[0] * Math.PI) / 180;
  const dlat = ((b[0] - a[0]) * Math.PI) / 180;
  const dlng = ((b[1] - a[1]) * Math.PI) / 180;
  const h =
    Math.sin(dlat / 2) ** 2 +
    Math.cos(rlat1) * Math.cos(rlat2) * Math.sin(dlng / 2) ** 2;
  return EARTH_R * 2 * Math.atan2(Math.sqrt(h), Math.sqrt(1 - h));
}

// Linear interpolation between two points (t in [0,1]).
export function lerp(a: LatLng, b: LatLng, t: number): LatLng {
  return [a[0] + (b[0] - a[0]) * t, a[1] + (b[1] - a[1]) * t];
}

// Bearing in degrees (0 = north, clockwise) — used to rotate the car icon.
export function bearing(a: LatLng, b: LatLng): number {
  const φ1 = (a[0] * Math.PI) / 180;
  const φ2 = (b[0] * Math.PI) / 180;
  const λ1 = (a[1] * Math.PI) / 180;
  const λ2 = (b[1] * Math.PI) / 180;
  const y = Math.sin(λ2 - λ1) * Math.cos(φ2);
  const x = Math.cos(φ1) * Math.sin(φ2) - Math.sin(φ1) * Math.cos(φ2) * Math.cos(λ2 - λ1);
  return ((Math.atan2(y, x) * 180) / Math.PI + 360) % 360;
}

// Build a coarse "route" between two points: a few jittered intermediate
// waypoints so the drawn polyline and driven path look road-like rather than a
// perfectly straight line. Deterministic-ish; good enough for a demo.
export function buildRoute(a: LatLng, b: LatLng, segments = 6): LatLng[] {
  const pts: LatLng[] = [a];
  const perpJitter = 0.0015;
  for (let i = 1; i < segments; i++) {
    const t = i / segments;
    const base = lerp(a, b, t);
    // Offset perpendicular-ish, tapering to 0 at the endpoints.
    const taper = Math.sin(t * Math.PI);
    const jitter = (Math.sin(t * 7.3 + a[0]) * perpJitter) * taper;
    pts.push([base[0] + jitter, base[1] - jitter]);
  }
  pts.push(b);
  return pts;
}

// Total length of a polyline in metres.
export function routeLength(pts: LatLng[]): number {
  let d = 0;
  for (let i = 1; i < pts.length; i++) d += haversine(pts[i - 1], pts[i]);
  return d;
}

// Position along a polyline at distance `dist` metres from the start, plus the
// bearing at that point. Clamps at the end.
export function pointAtDistance(pts: LatLng[], dist: number): { pos: LatLng; brg: number; done: boolean } {
  if (pts.length < 2) return { pos: pts[0] ?? [0, 0], brg: 0, done: true };
  let acc = 0;
  for (let i = 1; i < pts.length; i++) {
    const seg = haversine(pts[i - 1], pts[i]);
    if (acc + seg >= dist) {
      const t = seg === 0 ? 0 : (dist - acc) / seg;
      return { pos: lerp(pts[i - 1], pts[i], t), brg: bearing(pts[i - 1], pts[i]), done: false };
    }
    acc += seg;
  }
  return { pos: pts[pts.length - 1], brg: bearing(pts[pts.length - 2], pts[pts.length - 1]), done: true };
}

// Cumulative distance array for a polyline: cum[i] = metres from pts[0] to
// pts[i] (cum[0] === 0). Precompute once per leg so per-frame lookups along a
// dense OSRM polyline are O(log n) (binary search) instead of O(n).
export function cumulativeDistances(pts: LatLng[]): number[] {
  const cum = new Array<number>(pts.length);
  cum[0] = 0;
  for (let i = 1; i < pts.length; i++) cum[i] = cum[i - 1] + haversine(pts[i - 1], pts[i]);
  return cum;
}

// Position + bearing at `dist` metres along a polyline, using a precomputed
// cumulative-distance array. Binary-searches the segment; clamps at both ends.
export function pointAtDistanceCum(
  pts: LatLng[],
  cum: number[],
  dist: number,
): { pos: LatLng; brg: number; done: boolean } {
  if (pts.length < 2) return { pos: pts[0] ?? [0, 0], brg: 0, done: true };
  const total = cum[cum.length - 1];
  if (dist <= 0) return { pos: pts[0], brg: bearing(pts[0], pts[1]), done: false };
  if (dist >= total) {
    const n = pts.length;
    return { pos: pts[n - 1], brg: bearing(pts[n - 2], pts[n - 1]), done: true };
  }
  // First index i with cum[i] > dist → target segment is (i-1, i).
  let lo = 1;
  let hi = pts.length - 1;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    if (cum[mid] <= dist) lo = mid + 1;
    else hi = mid;
  }
  const i = lo;
  const seg = cum[i] - cum[i - 1];
  const t = seg === 0 ? 0 : (dist - cum[i - 1]) / seg;
  return { pos: lerp(pts[i - 1], pts[i], t), brg: bearing(pts[i - 1], pts[i]), done: false };
}

// Normalise an angle delta to the shortest signed arc in [-180, 180). Used to
// rotate the car marker the short way (avoid 350° → 10° spinning ~340°).
export function shortestAngleDelta(from: number, to: number): number {
  return (((to - from) % 360) + 540) % 360 - 180;
}

export function formatKm(m: number): string {
  return m >= 1000 ? `${(m / 1000).toFixed(1)} km` : `${Math.round(m)} m`;
}

export function formatDuration(s: number): string {
  const min = Math.round(s / 60);
  return min <= 1 ? "1 min" : `${min} min`;
}

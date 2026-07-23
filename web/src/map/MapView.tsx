import { useEffect, useMemo, useRef, useState } from "react";
import { MapContainer, Marker, Polyline, TileLayer, useMap, useMapEvents } from "react-leaflet";
import L from "leaflet";
import type { LatLng } from "../lib/geo";
import { lerp, shortestAngleDelta } from "../lib/geo";

export interface BotMarker {
  id: string;
  pos: LatLng;
}

interface MapViewProps {
  center: LatLng;
  pickup?: LatLng | null;
  drop?: LatLng | null;
  route?: LatLng[] | null;
  // The tracked car (assigned driver, from SSE) — animated smoothly.
  car?: LatLng | null;
  carBearing?: number;
  bots?: BotMarker[];
  // Click-to-set: which leg the next map click assigns (rider only).
  picking?: "pickup" | "drop" | null;
  onPick?: (p: LatLng) => void;
  // Keep these points in view.
  fitPoints?: LatLng[];
  // Rider: car target arrives ~1/s via SSE → interpolate. Driver: position is
  // already advanced at 60fps by a rAF loop → render it directly (no re-lerp).
  animateCar?: boolean;
}

function divIcon(html: string, className: string, size = 30): L.DivIcon {
  return L.divIcon({
    html,
    className,
    iconSize: [size, size],
    iconAnchor: [size / 2, size / 2],
  });
}

const pickupIcon = divIcon('<div class="pickup-dot"></div>', "", 16);
const dropIcon = divIcon("🏁", "pin-icon");
const botIcon = divIcon('<div class="bot-dot"></div>', "", 8);

// Inline SVG car glyph whose natural orientation points UP (north): a
// rounded car-top silhouette with a windshield wedge at the front. Because the
// art already faces north, `rotate(bearingDeg)` orients it correctly by
// construction (unlike the 🚗 emoji, which faces west and needs a fudge).
const CAR_SVG = `
<svg viewBox="0 0 24 24" width="26" height="26" aria-hidden="true">
  <g fill="var(--accent)" stroke="#fff" stroke-width="1.1" stroke-linejoin="round">
    <path d="M12 1.6c-2.5 0-3.9 1.7-4.3 4.2l-.7 5.1-.4 6.7c-.05 1.9 1.1 3 2.4 3.4 1 .3 1.9.4 3 .4s2-.1 3-.4c1.3-.4 2.45-1.5 2.4-3.4l-.4-6.7-.7-5.1C15.9 3.3 14.5 1.6 12 1.6Z"/>
    <path d="M12 3.7c1.5 0 2.5 1 2.9 2.7l.5 2.8c-1-.5-2.1-.8-3.4-.8s-2.4.3-3.4.8l.5-2.8C9.5 4.7 10.5 3.7 12 3.7Z" fill="#fff" stroke="none" opacity="0.9"/>
  </g>
</svg>`;

function carDivIcon(brg: number): L.DivIcon {
  return divIcon(`<div class="car-icon" style="transform:rotate(${brg}deg)">${CAR_SVG}</div>`, "", 30);
}

// Reads the panel's overlay card (`.float-card`) live and returns Leaflet
// padding that keeps fitted content / the followed car clear of both the card
// (bottom) and the persona chip (top-left). Re-measured at every call so a
// collapsed/expanded card immediately changes the usable region.
function cardPadding(map: L.Map): { paddingTopLeft: L.PointExpression; paddingBottomRight: L.PointExpression } {
  const panel = map.getContainer().closest(".panel");
  const card = panel?.querySelector<HTMLElement>(".float-card");
  const cardH = card ? card.offsetHeight : 0;
  return {
    paddingTopLeft: [16, 72], // clear the top-left persona chip
    paddingBottomRight: [16, cardH + 28], // clear the bottom overlay card
  };
}

// Unwraps a target bearing into a continuous angle so CSS transitions always
// rotate the marker the short way (never spin 340° the long way round).
function useUnwrappedAngle(target: number): number {
  const [angle, setAngle] = useState(target);
  const ref = useRef(target);
  useEffect(() => {
    const next = ref.current + shortestAngleDelta(ref.current, target);
    ref.current = next;
    setAngle(next);
  }, [target]);
  return angle;
}

function ClickCatcher({ picking, onPick }: { picking?: "pickup" | "drop" | null; onPick?: (p: LatLng) => void }) {
  useMapEvents({
    click(e) {
      if (picking && onPick) onPick([e.latlng.lat, e.latlng.lng]);
    },
  });
  return null;
}

// Keeps Leaflet's internal size in sync with the panel: the full-viewport
// layout sizes panels with flex/grid AFTER first paint, and Leaflet only
// measures its container once at init — without this, tiles render for the
// stale (smaller) size and most of the map stays blank.
function InvalidateOnResize() {
  const map = useMap();
  useEffect(() => {
    const el = map.getContainer();
    const ro = new ResizeObserver(() => map.invalidateSize());
    ro.observe(el);
    map.invalidateSize();
    return () => ro.disconnect();
  }, [map]);
  return null;
}

// Fits the map to the given points, keeping them inside the card-free region
// (obstruction-aware padding). Re-fits whenever the points change materially
// AND whenever the overlay card resizes (collapse/expand, phase change) so the
// map reclaims space the moment the card shrinks.
function FitBounds({ points }: { points: LatLng[] }) {
  const map = useMap();
  const key = points.map((p) => p.join(",")).join("|");
  const ptsRef = useRef(points);
  ptsRef.current = points;

  const fit = useRef((animate: boolean) => {
    const pts = ptsRef.current;
    if (pts.length === 0) return;
    const pad = cardPadding(map);
    if (pts.length === 1) {
      map.setView(pts[0], Math.max(map.getZoom(), 14), { animate });
      return;
    }
    const b = L.latLngBounds(pts.map((p) => L.latLng(p[0], p[1])));
    map.fitBounds(b, { ...pad, maxZoom: 15, animate });
  });

  useEffect(() => {
    fit.current(true);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key]);

  // Re-fit when the overlay card changes size (minimize/expand, content phase).
  useEffect(() => {
    const panel = map.getContainer().closest(".panel");
    const card = panel?.querySelector<HTMLElement>(".float-card");
    if (!card) return;
    let first = true;
    const ro = new ResizeObserver(() => {
      if (first) {
        first = false; // ignore the initial observe callback
        return;
      }
      fit.current(true);
    });
    ro.observe(card);
    return () => ro.disconnect();
  }, [map]);

  return null;
}

// The tracked car marker. Rider (animatePos=true) interpolates each SSE target
// over ~1050ms — slightly longer than the ~1/s update interval so a new target
// arrives while the previous tween is still running, chaining from the current
// animated position with no dead gap. Driver (animatePos=false) is already
// advanced at 60fps upstream, so it renders at the target directly.
function TrackedCar({ target, brg, animatePos }: { target: LatLng; brg: number; animatePos: boolean }) {
  const [pos, setPos] = useState<LatLng>(target);
  const fromRef = useRef<LatLng>(target);
  const posRef = useRef<LatLng>(target);
  const rafRef = useRef<number>();

  useEffect(() => {
    if (!animatePos) return;
    const from = fromRef.current;
    const start = performance.now();
    const dur = 1050;
    const step = (now: number) => {
      const t = Math.min(1, (now - start) / dur);
      const p = lerp(from, target, t);
      posRef.current = p;
      setPos(p);
      if (t < 1) rafRef.current = requestAnimationFrame(step);
      else fromRef.current = target;
    };
    rafRef.current = requestAnimationFrame(step);
    return () => {
      if (rafRef.current) cancelAnimationFrame(rafRef.current);
      fromRef.current = posRef.current; // chain the next tween from here
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [target[0], target[1], animatePos]);

  const angle = useUnwrappedAngle(brg);
  const icon = useMemo(() => carDivIcon(angle), [angle]);
  return <Marker position={animatePos ? pos : target} icon={icon} />;
}

// Keeps the moving car inside the padded (card-free) viewport: pans only when
// the car would drift under a card or off-screen. Throttled to ≤1/s and
// suppressed while the user is dragging the map.
function CarFollow({ pos }: { pos: LatLng }) {
  const map = useMap();
  const draggingRef = useRef(false);
  const lastRef = useRef(0);

  useEffect(() => {
    const onStart = () => {
      draggingRef.current = true;
    };
    const onEnd = () => {
      draggingRef.current = false;
    };
    map.on("dragstart", onStart);
    map.on("dragend", onEnd);
    return () => {
      map.off("dragstart", onStart);
      map.off("dragend", onEnd);
    };
  }, [map]);

  useEffect(() => {
    if (draggingRef.current) return;
    const now = performance.now();
    if (now - lastRef.current < 1000) return;
    lastRef.current = now;
    map.panInside(L.latLng(pos[0], pos[1]), cardPadding(map));
  }, [pos[0], pos[1], map]);

  return null;
}

export function MapView(props: MapViewProps) {
  const { center, pickup, drop, route, car, carBearing = 0, bots = [], picking, onPick, fitPoints, animateCar = true } = props;

  return (
    // fadeAnimation off: tile fade depends on requestAnimationFrame, which
    // backgrounded/embedded tabs throttle — tiles could stick at opacity 0.
    // Instant tiles are also the right feel for a console UI.
    <MapContainer center={center} zoom={13} zoomControl={false} attributionControl={false} fadeAnimation={false} className="leaflet-container">
      <InvalidateOnResize />
      <TileLayer
        // CartoDB Positron (light) tiles — OpenStreetMap data, free, no API key.
        // OSM + CARTO attribution kept per tile usage policy.
        url="https://{s}.basemaps.cartocdn.com/light_all/{z}/{x}/{y}{r}.png"
        attribution="&copy; OpenStreetMap contributors &copy; CARTO"
        subdomains="abcd"
        maxZoom={19}
        // Plain OSM fallback (no key): uncomment to swap tile provider.
        // url="https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png"
        // attribution="&copy; OpenStreetMap contributors"
      />
      <ClickCatcher picking={picking} onPick={onPick} />

      {route && route.length > 1 && (
        <Polyline
          positions={route}
          pathOptions={{ color: "#6d5ce6", weight: 4, opacity: 0.85, dashArray: "8 8", lineCap: "round" }}
        />
      )}

      {pickup && <Marker position={pickup} icon={pickupIcon} />}
      {drop && <Marker position={drop} icon={dropIcon} />}

      {bots.map((b) => (
        <Marker key={b.id} position={b.pos} icon={botIcon} />
      ))}

      {car && (
        <>
          <TrackedCar target={car} brg={carBearing} animatePos={animateCar} />
          <CarFollow pos={car} />
        </>
      )}

      {fitPoints && fitPoints.length > 0 && <FitBounds points={fitPoints} />}
    </MapContainer>
  );
}

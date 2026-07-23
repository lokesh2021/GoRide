import { useEffect, useMemo, useRef, useState } from "react";
import { MapContainer, Marker, Polyline, TileLayer, useMap, useMapEvents } from "react-leaflet";
import L from "leaflet";
import type { LatLng } from "../lib/geo";
import { lerp } from "../lib/geo";

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

// Fits the map to the given points whenever they change materially.
function FitBounds({ points }: { points: LatLng[] }) {
  const map = useMap();
  const key = points.map((p) => p.join(",")).join("|");
  useEffect(() => {
    if (points.length === 0) return;
    if (points.length === 1) {
      map.setView(points[0], Math.max(map.getZoom(), 14), { animate: true });
      return;
    }
    const b = L.latLngBounds(points.map((p) => L.latLng(p[0], p[1])));
    map.fitBounds(b, { padding: [50, 50], maxZoom: 15, animate: true });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key]);
  return null;
}

// A marker that smoothly interpolates to its target position over ~900ms
// (matching the ~1/sec driver_location cadence) so movement looks continuous.
function AnimatedCar({ target, brg }: { target: LatLng; brg: number }) {
  const [pos, setPos] = useState<LatLng>(target);
  const fromRef = useRef<LatLng>(target);
  const rafRef = useRef<number>();

  useEffect(() => {
    const from = fromRef.current;
    const start = performance.now();
    const dur = 900;
    const step = (now: number) => {
      const t = Math.min(1, (now - start) / dur);
      const p = lerp(from, target, t);
      setPos(p);
      if (t < 1) rafRef.current = requestAnimationFrame(step);
      else fromRef.current = target;
    };
    rafRef.current = requestAnimationFrame(step);
    return () => {
      if (rafRef.current) cancelAnimationFrame(rafRef.current);
      fromRef.current = pos;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [target[0], target[1]]);

  const icon = useMemo(
    () => divIcon(`<div class="car-icon" style="transform:rotate(${brg}deg)">🚗</div>`, "", 30),
    [brg],
  );
  return <Marker position={pos} icon={icon} />;
}

export function MapView(props: MapViewProps) {
  const { center, pickup, drop, route, car, carBearing = 0, bots = [], picking, onPick, fitPoints } = props;

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

      {car && <AnimatedCar target={car} brg={carBearing} />}

      {fitPoints && fitPoints.length > 0 && <FitBounds points={fitPoints} />}
    </MapContainer>
  );
}

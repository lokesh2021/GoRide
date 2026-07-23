// API response/request types — one authoritative module, matching the Go
// handler JSON in internal/httpapi/* and the domain read models exactly.
// Money is integer paise (INR). Distances metres, durations seconds.

// ---- shared ----

export type Tier = "mini" | "sedan" | "xl";
export type PaymentMethod = "upi" | "card" | "cash";

// SPEC error envelope: {"error": {code, message}}.
export interface ErrorEnvelope {
  error: { code: string; message: string };
}

// ---- quotes (POST /v1/quotes) ----

export interface Coord {
  lat: number;
  lng: number;
}

export interface QuoteRequest {
  pickup: Coord;
  drop: Coord;
  city?: string;
}

// httpapi.quoteResponse
export interface QuoteResponse {
  quote_id: string;
  city: string;
  distance_m: number;
  duration_s: number;
  surge: number;
  surge_x100: number;
  prices: Record<Tier, number>; // tier -> paise
  expires_at: string;
}

// ---- rides ----

// rides.DriverCard
export interface DriverCard {
  name: string;
  vehicle_model: string;
  plate: string;
  rating: number;
}

// Ride lifecycle statuses (rides.Status).
export type RideStatus =
  | "REQUESTED"
  | "MATCHING"
  | "DRIVER_ASSIGNED"
  | "DRIVER_ARRIVING"
  | "ARRIVED"
  | "IN_PROGRESS"
  | "COMPLETED"
  | "CANCELLED_BY_RIDER"
  | "CANCELLED_BY_DRIVER"
  | "EXPIRED";

// rides.View — returned by create/get/cancel/arriving/arrived and by accept.
export interface RideView {
  id: string;
  rider_id: string;
  driver_id: string | null;
  quote_id: string;
  tier: Tier;
  status: RideStatus;
  pickup_lat: number;
  pickup_lng: number;
  drop_lat: number;
  drop_lng: number;
  payment_method: PaymentMethod | null;
  fare_total: number | null;
  cancel_reason: string | null;
  driver?: DriverCard;
  created_at: string;
  updated_at: string;
}

export interface RideRequest {
  quote_id: string;
  tier: Tier;
  payment_method: PaymentMethod;
}

// ---- trips ----

// pricing.Breakdown (fare line items; components are pre-surge paise).
// M13: the trip-end event and the immutable receipt breakdown also carry the
// trip metrics below. They are optional here because older receipts (written
// before M13) lack them — render gracefully when absent.
export interface FareBreakdown {
  base: number;
  distance_component: number;
  time_component: number;
  surge_x100: number;
  total: number;
  distance_m?: number;
  duration_s?: number;
  started_at?: string;
  ended_at?: string;
}

export type TripStatus = "STARTED" | "PAUSED" | "ENDED";

// trips.Trip
export interface Trip {
  ride_id: string;
  status: TripStatus;
  ride_status: RideStatus;
  started_at: string;
  ended_at?: string;
  paused_seconds: number;
  distance_m?: number;
  fare?: FareBreakdown;
}

// ---- payments ----

export type PaymentStatus = "PENDING" | "PROCESSING" | "SUCCEEDED" | "FAILED";

// payments.Payment
export interface Payment {
  id: string;
  ride_id: string;
  amount: number;
  method: PaymentMethod;
  status: PaymentStatus;
  psp_ref?: string;
  retry_count: number;
}

// payments.ReceiptView
export interface ReceiptView {
  breakdown: Record<string, unknown>;
  total: number;
  created_at: string;
}

// payments.HistoryItem
export interface HistoryItem {
  ride_id: string;
  status: RideStatus;
  tier: Tier;
  fare_total: number | null;
  pickup_lat: number;
  pickup_lng: number;
  drop_lat: number;
  drop_lng: number;
  created_at: string;
  driver?: { name: string; plate: string };
  receipt?: ReceiptView;
}

// GET /v1/riders/{id}/rides
export interface HistoryResponse {
  rides: HistoryItem[];
}

// ---- SSE envelope + event payloads (events.Envelope) ----

export type SSEEventType =
  | "ride.status_changed"
  | "ride.offer"
  | "ride.otp"
  | "ride.driver_location"
  | "payment.updated";

export interface SSEEnvelope<T = unknown> {
  type: SSEEventType | string;
  ride_id: string;
  data: T;
  ts: string;
}

// ride.status_changed — carries the new status; on assignment also a driver
// card; on completion also the fare breakdown.
export interface StatusChangedData {
  status: RideStatus;
  driver?: DriverCard;
  fare?: FareBreakdown;
}

// ride.offer (driver channel)
export interface OfferData {
  ride_id: string;
  tier: Tier;
  pickup_lat: number;
  pickup_lng: number;
  drop_lat: number;
  drop_lng: number;
  /** Quoted fare for the booked tier, integer paise. */
  fare: number;
  distance_m: number;
  duration_s: number;
  rider_name: string;
  rider_rating: number;
  expires_at: string;
}

// ride.otp (rider channel)
export interface OtpData {
  otp: string;
}

// ride.driver_location (ride channel)
export interface DriverLocationData {
  driver_id: string;
  lat: number;
  lng: number;
}

// payment.updated (ride channel)
export interface PaymentUpdatedData {
  status: PaymentStatus;
  retry_count: number;
}

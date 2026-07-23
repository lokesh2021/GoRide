// Thin fetch wrapper around the GoRide API. Every call carries a bearer token
// (the persona's seed token). Errors surface the SPEC error envelope's message
// so the UI can show the real backend reason.

import type {
  HistoryResponse,
  Payment,
  QuoteRequest,
  QuoteResponse,
  RideRequest,
  RideView,
  Trip,
} from "./types";

export class ApiError extends Error {
  code: string;
  status: number;
  constructor(status: number, code: string, message: string) {
    super(message);
    this.name = "ApiError";
    this.code = code;
    this.status = status;
  }
}

function uuid(): string {
  // Idempotency-Key generator. crypto.randomUUID is available in all modern
  // browsers served over http(s)/localhost.
  return crypto.randomUUID();
}

async function request<T>(
  token: string,
  method: string,
  path: string,
  body?: unknown,
  opts?: { idempotent?: boolean },
): Promise<T> {
  const headers: Record<string, string> = {
    Authorization: `Bearer ${token}`,
  };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (opts?.idempotent) headers["Idempotency-Key"] = uuid();

  const res = await fetch(path, {
    method,
    headers,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });

  const text = await res.text();
  const parsed = text ? JSON.parse(text) : {};

  if (!res.ok) {
    const env = parsed as { error?: { code?: string; message?: string } };
    throw new ApiError(
      res.status,
      env.error?.code ?? "UNKNOWN",
      env.error?.message ?? `request failed (${res.status})`,
    );
  }
  return parsed as T;
}

// Client bound to a single persona's token.
export class Api {
  constructor(private token: string) {}

  // ---- rider ----
  quote(req: QuoteRequest) {
    return request<QuoteResponse>(this.token, "POST", "/v1/quotes", req);
  }
  createRide(req: RideRequest) {
    return request<RideView>(this.token, "POST", "/v1/rides", req, { idempotent: true });
  }
  getRide(id: string) {
    return request<RideView>(this.token, "GET", `/v1/rides/${id}`);
  }
  cancelRide(id: string, reason: string) {
    return request<RideView>(this.token, "POST", `/v1/rides/${id}/cancel`, { reason });
  }
  pay(rideId: string) {
    return request<Payment>(this.token, "POST", "/v1/payments", { ride_id: rideId }, { idempotent: true });
  }
  history(riderId: string) {
    return request<HistoryResponse>(this.token, "GET", `/v1/riders/${riderId}/rides`);
  }

  // ---- driver ----
  setAvailability(driverId: string, available: boolean) {
    return request<{ driver_id: string; status: string }>(
      this.token,
      "POST",
      `/v1/drivers/${driverId}/availability`,
      { available },
    );
  }
  ping(driverId: string, lat: number, lng: number) {
    return request<{ ok: boolean }>(this.token, "POST", `/v1/drivers/${driverId}/location`, { lat, lng });
  }
  accept(driverId: string, rideId: string) {
    return request<RideView>(this.token, "POST", `/v1/drivers/${driverId}/accept`, { ride_id: rideId });
  }
  decline(driverId: string, rideId: string) {
    return request<{ ok: boolean }>(this.token, "POST", `/v1/drivers/${driverId}/decline`, { ride_id: rideId });
  }
  arriving(rideId: string) {
    return request<RideView>(this.token, "POST", `/v1/rides/${rideId}/arriving`);
  }
  arrived(rideId: string) {
    return request<RideView>(this.token, "POST", `/v1/rides/${rideId}/arrived`);
  }
  startTrip(rideId: string, otp: string) {
    return request<Trip>(this.token, "POST", `/v1/trips/${rideId}/start`, { otp });
  }
  endTrip(rideId: string) {
    return request<Trip>(this.token, "POST", `/v1/trips/${rideId}/end`, undefined, { idempotent: true });
  }
}

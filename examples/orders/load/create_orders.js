// k6 load script: each iteration creates an order and reads it back.
import http from 'k6/http';
import { check, sleep } from 'k6';

const API = __ENV.API;
const RUN = __ENV.RUN;

export default function () {
  const id = `load-${RUN}-${__VU}-${__ITER}`;
  const res = http.post(`${API}/orders`, JSON.stringify({ id }), { headers: { 'Content-Type': 'application/json' } });
  check(res, { 'created': (r) => r.status === 201 });
  const get = http.get(`${API}/orders/${id}`);
  check(get, { 'readable': (r) => r.status === 200 });
  sleep(0.1); // think time: ~10 orders/s per user. Without it the toy service is OOM killed at 64 MiB
}

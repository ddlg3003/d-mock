// k6 load script: each virtual user creates an order and checks it out, like a shopper.
import http from 'k6/http';
import { check, sleep } from 'k6';

const API = __ENV.API;
const json = { headers: { 'Content-Type': 'application/json' } };

export default function () {
  const id = `load-${__ENV.RUN}-${__VU}-${__ITER}`;
  const created = http.post(`${API}/orders`, JSON.stringify({ id, amount: 150000 }), json);
  check(created, { 'created': (r) => r.status === 201 });
  const res = http.post(`${API}/checkout`, JSON.stringify({ order_id: id }), json);
  check(res, { 'accepted': (r) => r.status === 202 });
  sleep(0.5); // think time: a real shopper doesn't click non-stop
}

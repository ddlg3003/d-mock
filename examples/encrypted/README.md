# Example: an API with encrypted payloads

A service (`service/`, a toy standing in for yours) asks a third-party API about a reference.
Every request and response body is encrypted, and the service must check that the decrypted
answer is about the reference it asked for. The API can also push an encrypted callback.

```
dmock test examples/encrypted
```

| Scenario | Question |
|---|---|
| `01_round_trip.yaml` | Does the encrypted round trip work, and did the API receive our ref? |
| `02_declined.yaml` | Is a DECLINED answer kept? |
| `03_answer_for_another_ref.yaml` | An answer that decrypts fine but is about another ref: is it refused? |
| `04_encrypted_callback.yaml` | A valid encrypted callback changes the status; a forged one is refused. |

How it's built:

- `mocks/secure-api.yaml` has a **codec**: d-mock decrypts each request before Mockoon sees it
  (so `key: "bodyRaw 'ref'"`, state and `mock calls` work on plain JSON) and encrypts each
  answer after templating (so it can echo the request's ref).
- Scenarios talk encrypted too: `http` with `encode` / `decode`.
- `codec/encrypt.sh` and `codec/decrypt.sh` use `openssl enc`; in your suite they'd call your
  real tool. `codec/demo.key` is a throwaway key for this example only.

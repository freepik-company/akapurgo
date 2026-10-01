# Akapurgo

<img src="https://raw.githubusercontent.com/dfradehubs/akapurgo/main/docs/img/logo.png" alt="Akapurgo Logo (Main) logo." width="150">

![GitHub Release](https://img.shields.io/github/v/release/dfradehubs/akapurgo)
![GitHub go.mod Go version](https://img.shields.io/github/go-mod/go-version/dfradehubs/akapurgo)
[![Go Report Card](https://goreportcard.com/badge/github.com/dfradehubs/akapurgo)](https://goreportcard.com/report/github.com/dfradehubs/akapurgo)
![GitHub License](https://img.shields.io/github/license/dfradehubs/akapurgo)

Akapurgo is a project that integrates with Akamai services and provides logging capabilities. This project is built using Go and includes configurations for server settings, Akamai credentials, and logging options.

## Table of Contents

- [Installation](#installation)
- [Configuration](#configuration)
- [Usage](#usage)
- [Logging](#logging)
- [Contributing](#contributing)
- [License](#license)

## Installation

To install the dependencies for this project, use the following commands:

```sh
go mod tidy
```

## Configuration
The configuration file config/samples/config.yaml includes the following settings:  
* **server**: Server settings including the listen address.
* **akamai**: Akamai credentials including host, client secret, client token, and access token.
* **logs**: Logging settings including access log fields.
Example configuration:
```yaml
server:
  listenAddress: "127.0.0.1:8080"
  #config:
  #  read_buffer_size: 16384
akamai:
  host: "https://akamai.example.com"
  clientsecret: "your-client-secret"
  clientToken: "your-client-token"
  accessToken: "your-access-token"
logs:
  show_access_logs: true
  # If you want to log an user from a JWT Token, you can enable the jwt_user option and set the header name
  #jwt_user:
  #  enabled: true
  #  header: "Test"
  # JWT field which you want to log
  #  jwt_field: "email"
  access_logs_fields:
    - REQUEST:method
    - REQUEST:host
    - REQUEST:path
    - REQUEST:proto
    - REQUEST:referer
    - REQUEST:body

    - REQUEST_HEADER:user-agent
    - REQUEST_HEADER:x-forwarded-for
    - REQUEST_HEADER:x-real-ip

    - RESPONSE:status

    - RESPONSE_HEADER:content-length
```

## Usage
To run the project, use the following command:
```sh
go run cmd/main.go
```

For purging content, the application provides a POST endpoint at `/api/v1/purge`. The request body should include the following fields:
```json
{
    "purgeType": "urls", // "urls" or "cache-tags"
    "actionType": "invalidate", // "invalidate" or "delete"
    "environment": "production", // "production" or "staging"
    "originPurgeRequest": true,
    "paths": [ // List of paths to purge or cache tags to delete (depending on the purgeType)
      "https://img.example.com/path1.jpg",
      "https://img.example.com/path2.jpg"
    ]
}
```

When `originPurgeRequest` and `post_purge_request.enabled` are true, Akapurgo sends
the configured request to every URL before calling the Akamai purge API. This
allows an Akamai property to bypass its edge cache, resolve the public URL to
the corresponding storage headers and evict a private origin cache first. If
that request fails, Akapurgo still attempts the Akamai purge, then returns HTTP
502 with the outcome of each stage. This is a best-effort edge eviction: it does
not confirm removal from the origin, and the edge may refill from stale origin
content. Callers must retry the complete operation after fixing the origin
failure and verify that the public content is no longer served. A CCU `201`
means the request was accepted, not that the purge has finished.

Origin requests require HTTP 200 or 204 and two confirmation headers. Their
names and expected value are configurable under `post_purge_request`:

```yaml
post_purge_request:
  confirmation:
    request_id_header: "X-Purge-Request-Id"
    response_header: "X-Origin-Purge"
    response_value: "complete"
```

These are the defaults when the settings are omitted. Set them to match the
protocol implemented by your origin. This feature does not require a specific
storage provider, proxy implementation, or number of cache nodes.

* The origin must return the configured `response_header` exactly once, with
  the exact `response_value`, only after completing the requested purge.
* The `request_id_header` carries a fresh 32-character lowercase hexadecimal ID
  generated for each URL and attempt. The origin must echo that header exactly
  once with the same ID. Akapurgo overrides any fixed value for this header in
  `post_purge_request.headers`.

Ordinary content, missing, duplicate or stale confirmations, a different
confirmation value, and legacy 404/412 responses are failures. Akamai CCU is
still attempted and the response reports HTTP 502 with the origin failure.
Invalid confirmation configuration returns HTTP 500 before either stage sends
requests. Header names must be valid and distinct (case-insensitively); the
confirmation value must be printable ASCII without surrounding whitespace.

**Rollout requirement:** implement the confirmation protocol at the origin and
configure any intermediaries before upgrading Akapurgo. Forward the request ID,
preserve both response headers, and bypass caching and content failover for
purge requests. Intermediaries must not manufacture confirmation headers. An
endpoint that does not implement the configured protocol will fail confirmation;
retry after all relevant endpoints have been updated.

Confirmation records completion for that attempt. It does not prevent later
cache refills or concurrent writes. Update or remove the source content first,
and coordinate outstanding writes when a durable removal is required.

URLs must use HTTPS and match the configured
`post_purge_request.allowed_hosts` allowlist. All origin URLs are validated
before either stage sends requests. Invalid URLs or more than 100 origin URLs
return HTTP 400 with the failing index or limit; an invalid allowlist configuration
returns HTTP 500. Adding a new CDN hostname requires explicitly updating the
deployment's allowlist, and the hostname must have the correct Akamai purge rule.
The legacy `postPurgeRequest` field remains accepted for backward compatibility.

For example, an origin 403 followed by an accepted CCU request returns HTTP 502:

```json
{
  "error": "Failed to purge origin cache",
  "origin": {
    "status": "failed",
    "failures": [
      {
        "index": 0,
        "host": "img.example.com",
        "httpStatus": 403,
        "reason": "Origin returned an unsuccessful status"
      }
    ]
  },
  "akamai": {
    "status": "accepted",
    "httpStatus": 201,
    "response": {"httpStatus": 201, "detail": "Request accepted"}
  }
}
```

The `akamai.status` in a partial failure is `accepted`, `failed` (CCU did not
accept the request), `unknown` (transport or response decoding failure), or
`not_attempted` (request construction or signing failure). `akamai.httpStatus`
is the actual upstream HTTP status, or 0 if no response was received. Origin
failures report the submitted URL index and hostname without query strings,
credentials or upstream response bodies. Requests without an origin failure
keep the existing Akamai response format.

## Logging
The project includes extensive logging capabilities. The logs can be configured in the config.yaml file under the logs section.  Example log fields:  
* REQUEST:method: HTTP method of the request.
* REQUEST:host: Host of the request.
* REQUEST:path: Path of the request.
* REQUEST:proto: Protocol of the request.
* REQUEST:referer: Referer of the request.
* REQUEST:body: Body of the request.
* REQUEST_HEADER:user-agent: User-Agent header of the request.
* REQUEST_HEADER:x-forwarded-for: X-Forwarded-For header of the request.
* REQUEST_HEADER:x-real-ip: X-Real-IP header of the request.
* RESPONSE:status: HTTP status of the response.
* RESPONSE_HEADER:content-length: Content-Length header of the response.

> Note:
You can log any header or field from the request or response by adding it to the access_logs_fields list in the config.yaml file. The logs will be printed to the console.

## Contributing
Contributions are welcome! Please open an issue or submit a pull request.  

## License
This project is licensed under the Apache v2.0 License.

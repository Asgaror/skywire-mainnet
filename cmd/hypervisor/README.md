# Hypervisor

Hypervisor exposes visor management operations via web API.

**Generate config file:**

```bash
$ hypervisor gen-config
```

**Run with mock data:**

```bash
$ hypervisor --mock
```

By default, the RESTful API is served on `:8000`.

## Endpoints Documentation

Endpoints are documented in the provided [Postman](https://www.getpostman.com/) file: `hypervisor.postman_collection.json`.

## Web UI

UI is served on the same port as the API (`:8000` by default). Directory to search for the build frontend is passed in the `web_dir` field of the hypervisor's config.

### Authentication information

- When the authentication cookie is invalid, the hypervisor will return code `401`.
- The default authentication cookie timeout is 12 hours. This can be configured in the hypervisor config file: `cookies.expires_duration`.
- There is currently no enforcement of when a user should change their password.
# Tone examples

Paired rewrites. The "after" column is the published voice.

| Bad | Good |
|---|---|
| In this post, we'll explore how prompt caching changes economics. | Prompt caching changes agent economics in three specific ways. |
| It's worth noting that the cache TTL is 5 minutes. | The cache TTL is 5 minutes. |
| You can easily integrate this by simply calling the SDK. | The SDK exposes a single `cache_control` parameter; set it per message. |
| There are many best practices for this. | Three practices recover most of the cost: A, B, C. |
| This is a robust, go-to solution for production workloads. | This survives 50k QPS in our prod cluster. |
| Doesn't it make sense to cache the system prompt? | The system prompt is the longest reusable chunk; cache it first. |

## What changed

In every "good" version: a hedge or generalization was replaced with a number, a system name, or a measured outcome. If a sentence can be sharpened by replacing "many" / "easily" / "best" with a specific value or system, sharpen it.

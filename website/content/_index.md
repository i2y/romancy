---
title: "Romancy"
layout: "hextra-home"
---

{{< hextra/hero-badge >}}
  <div class="hx-w-2 hx-h-2 hx-rounded-full hx-bg-primary-400"></div>
  <span>Durable Execution for Go</span>
  {{< icon name="arrow-circle-right" attributes="height=14" >}}
{{< /hextra/hero-badge >}}

<div class="hx-mt-6 hx-mb-6">
{{< hextra/hero-headline >}}
  Lightweight Durable Execution&nbsp;<br class="sm:hx-block hx-hidden" />Framework for Go
{{< /hextra/hero-headline >}}
</div>

<div class="hx-mb-12">
{{< hextra/hero-subtitle >}}
  Build long-running workflows that survive crashes&nbsp;<br class="sm:hx-block hx-hidden" />without separate infrastructure
{{< /hextra/hero-subtitle >}}
</div>

<div class="hx-mb-6">
{{< hextra/hero-button text="Get Started" link="docs/getting-started/installation" >}}
{{< hextra/hero-button text="GitHub" link="https://github.com/i2y/romancy" style="outline" >}}
</div>

<div class="hx-mt-6"></div>

{{< hextra/feature-grid >}}
  {{< hextra/feature-card
    title="Lightweight Library"
    subtitle="Runs in your application process - no separate workflow server infrastructure required."
    class="hx-aspect-auto md:hx-aspect-[1.1/1] max-md:hx-min-h-[340px]"
    style="background: radial-gradient(ellipse at 50% 80%,rgba(194,97,254,0.15),hsla(0,0%,100%,0));"
  >}}
  {{< hextra/feature-card
    title="Durable Execution"
    subtitle="Deterministic replay with workflow history for automatic crash recovery."
    class="hx-aspect-auto md:hx-aspect-[1.1/1] max-lg:hx-min-h-[340px]"
    style="background: radial-gradient(ellipse at 50% 80%,rgba(142,53,234,0.15),hsla(0,0%,100%,0));"
  >}}
  {{< hextra/feature-card
    title="Saga Pattern"
    subtitle="Automatic compensation on failure with WithCompensation option."
    class="hx-aspect-auto md:hx-aspect-[1.1/1] max-md:hx-min-h-[340px]"
    style="background: radial-gradient(ellipse at 50% 80%,rgba(221,214,254,0.15),hsla(0,0%,100%,0));"
  >}}
  {{< hextra/feature-card
    title="Multi-worker Execution"
    subtitle="Run workflows safely across multiple servers or containers with database locking."
  >}}
  {{< hextra/feature-card
    title="CloudEvents Support"
    subtitle="Native support for CloudEvents protocol and HTTP binding specification."
  >}}
  {{< hextra/feature-card
    title="MCP Integration"
    subtitle="Expose workflows as MCP tools for AI assistants like Claude Desktop."
  >}}
{{< /hextra/feature-grid >}}

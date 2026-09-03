---
title: Cookbook
weight: 60
description: Step-by-step recipes for the hosts PHP-FPM is usually found on.
---

# Cookbook

Each recipe walks one kind of host from install to a daemon in apply mode, with the commands and what they print. Read the one that matches your host.

- **[Forge and Ploi](forge-and-ploi.md)**: a pool per site, MySQL on the same host, no cgroup limit on php-fpm.
- **[Two PHP versions](two-php-versions.md)**: two masters on one host, one daemon each, and how to split the budget.

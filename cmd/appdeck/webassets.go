// The dashboard's own web identity: an SVG favicon (the terminal glyph on
// the accent tile) and a minimal PWA manifest — browser tabs and phone home
// screens show the product instead of a blank tab icon.
package main

const faviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><rect width="32" height="32" rx="7" fill="#2d81ff"/><path d="M9 11l5 5-5 5" stroke="#fff" stroke-width="2.6" fill="none" stroke-linecap="round" stroke-linejoin="round"/><line x1="16" y1="21" x2="23" y2="21" stroke="#fff" stroke-width="2.6" stroke-linecap="round"/></svg>`

const siteWebManifest = `{"name":"AppDeck","short_name":"AppDeck","description":"One page for every local app — start, stop, open","start_url":"/","display":"standalone","background_color":"#0b0b0c","theme_color":"#0b0b0c","icons":[{"src":"/favicon.svg","sizes":"any","type":"image/svg+xml","purpose":"any"}]}`

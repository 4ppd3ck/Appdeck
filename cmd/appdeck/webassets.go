// The dashboard's web identity and minimal PWA manifest.
package main

const faviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"><rect x=".5" y=".5" width="63" height="63" rx="15" fill="#1b1b38" stroke="#484578"/><g transform="translate(12.8 12.8) scale(1.6)" fill="none" stroke="#9a91ff" stroke-width="1.15" stroke-linecap="round" stroke-linejoin="round"><path d="M4.6 16.6c-1.4 1.2-2.1 4.8-2.1 4.8s3.6-.7 4.8-2.1a2 2 0 0 0-2.7-2.7Z"/><path d="m12 15-3-3c1.6-3.8 4.7-7.2 12-9-.6 7.3-4 10.4-7.8 12Z"/><path d="M9 12H4.5s.6-3.3 4.1-4.5M12 15v4.5s3.3-.6 4.5-4.1"/><circle cx="16.5" cy="7.5" r="1"/></g></svg>`

const siteWebManifest = `{"name":"AppDeck","short_name":"AppDeck","description":"One page for every local app — start, stop, open","start_url":"/","display":"standalone","background_color":"#0b0b0c","theme_color":"#0b0b0c","icons":[{"src":"/favicon.svg","sizes":"any","type":"image/svg+xml","purpose":"any"}]}`

# website/

The docs site at <https://community.tuskira.ai>: a [Docusaurus](https://docusaurus.io) shell around the repo's `docs/` folder. The content lives in `docs/` only; nothing here duplicates it.

```sh
cd website
npm ci
npm start          # live preview at http://localhost:3000
npm run build      # what CI runs; a broken link or anchor fails the build
```

`.github/workflows/docs.yml` builds the site on pull requests that touch `docs/` or `website/`, and builds + publishes it to GitHub Pages on every merge to `main`. `static/CNAME` pins the custom domain.

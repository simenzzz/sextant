"""HTTP routes, one module per resource.

Routers live here rather than inside create_app so main.py stays an assembly
point: it decides what is mounted and under which middleware, and never what a
route does.
"""
